package service

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/rules"
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// Rule sets.
//
// The live rule set is an atomic.Pointer to an immutable ruleVersion. A
// request loads it once and uses that version throughout, so a deploy never
// takes a lock on the scoring path and never mixes two versions in one
// decision. Deploys are serialized by a mutex, and every version stays in
// memory (and, with a history directory, on disk) so any logged decision
// can be replayed against the exact rules that made it.

// ruleVersion is one deployed rule set with its per-rule counters.
type ruleVersion struct {
	Version    uint64
	Text       string
	ListsJSON  json.RawMessage // canonical, as deployed; null if none
	Set        *rules.RuleSet
	DeployedAt time.Time

	// Indexed by CompiledRule.Index.
	reason []string         // "Matched rule: ..." for the response
	count  []*atomic.Uint64 // live rules: decisions made, shared by rule id across versions
	shadow []*shadowStat    // shadow rules: online match counts
	live   []*shadowStat    // non-nil entries of shadow, for the per-request loop
}

// shadowStat counts a shadow rule's online matches since it was deployed.
// It follows the rule by id across versions: redeploying the same rule
// keeps counting, dropping it and adding it back starts over.
type shadowStat struct {
	ID, Text     string
	Action       string
	Since        time.Time
	SinceVersion uint64
	versions     map[uint64]bool // versions it has been live in, guarded by ruleStore.mu

	evaluated, matched atomic.Uint64
}

type ruleStore struct {
	cat *schema.Catalog
	dir string // rule history directory; "" keeps history in memory only
	now func() time.Time

	cur atomic.Pointer[ruleVersion]

	mu       sync.Mutex
	history  map[uint64]*ruleVersion
	counters map[string]*atomic.Uint64 // live rule id -> decisions
	actions  map[string]string         // live rule id -> action, for metrics
	shadows  map[string]*shadowStat
}

func newRuleStore(cat *schema.Catalog, dir string, now func() time.Time) *ruleStore {
	return &ruleStore{
		cat: cat, dir: dir, now: now,
		history:  map[uint64]*ruleVersion{},
		counters: map[string]*atomic.Uint64{},
		actions:  map[string]string{},
		shadows:  map[string]*shadowStat{},
	}
}

func (s *ruleStore) current() *ruleVersion { return s.cur.Load() }

// env returns the environment rules are checked in: the catalog and the
// named lists.
func (s *ruleStore) env(lists rules.Lists) rules.Env {
	return rules.Env{Catalog: s.cat, Lists: lists}
}

// compile checks and compiles a rule set without deploying it. The error
// is a rules.Diagnostics for problems in the text, or a plain error for bad
// lists JSON.
func (s *ruleStore) compile(text string, listsJSON []byte, version uint64) (*rules.RuleSet, json.RawMessage, error) {
	var lists rules.Lists
	var canon json.RawMessage = []byte("null")
	if len(listsJSON) > 0 && string(listsJSON) != "null" {
		var err error
		if lists, err = rules.ParseLists(listsJSON); err != nil {
			return nil, nil, &listsError{err}
		}
		if canon, err = canonicalLists(listsJSON); err != nil {
			return nil, nil, &listsError{err}
		}
	}
	rs, err := rules.Load(text, s.env(lists), version)
	return rs, canon, err
}

// listsError reports named lists that are not valid JSON lists: a client
// error, unlike a failure to save the rules.
type listsError struct{ err error }

func (e *listsError) Error() string { return e.err.Error() }
func (e *listsError) Unwrap() error { return e.err }

// deploy compiles text and makes it the live rule set with the next
// version. With a history directory, the version is written there before it
// goes live, so no decision is ever made by rules the audit cannot find.
func (s *ruleStore) deploy(text string, listsJSON []byte) (*ruleVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := uint64(1)
	if cur := s.cur.Load(); cur != nil {
		next = cur.Version + 1
	}
	for v := range s.history {
		next = max(next, v+1)
	}
	rs, canon, err := s.compile(text, listsJSON, next)
	if err != nil {
		return nil, err
	}
	if err := s.persist(next, text, canon); err != nil {
		return nil, fmt.Errorf("rules: saving version %d: %w", next, err)
	}
	rv := s.install(next, text, canon, rs, s.now())
	s.cur.Store(rv)
	return rv, nil
}

// install builds a ruleVersion's counters and records it in the history.
// Caller holds mu.
func (s *ruleStore) install(version uint64, text string, canon json.RawMessage, rs *rules.RuleSet, at time.Time) *ruleVersion {
	rv := &ruleVersion{
		Version: version, Text: text, ListsJSON: canon, Set: rs, DeployedAt: at,
		reason: make([]string, len(rs.Rules)),
		count:  make([]*atomic.Uint64, len(rs.Rules)),
		shadow: make([]*shadowStat, len(rs.Rules)),
	}
	prev := s.cur.Load()
	for _, r := range rs.Rules {
		if r.Shadow {
			st := s.shadows[r.ID]
			// Keep counting only if the rule is live right now; a rule
			// that was dropped and is coming back starts a new period.
			if st == nil || prev == nil || !st.versions[prev.Version] {
				st = &shadowStat{ID: r.ID, Text: r.Text, Action: r.Action.String(), Since: at, SinceVersion: version, versions: map[uint64]bool{}}
				s.shadows[r.ID] = st
			}
			st.versions[version] = true
			rv.shadow[r.Index] = st
			rv.live = append(rv.live, st)
			continue
		}
		verb := "Matched rule"
		if r.Action == rules.Allow {
			verb = "Allowed by rule"
		}
		rv.reason[r.Index] = verb + ": " + r.Text
		c := s.counters[r.ID]
		if c == nil {
			c = new(atomic.Uint64)
			s.counters[r.ID] = c
			s.actions[r.ID] = r.Action.String()
		}
		rv.count[r.Index] = c
	}
	s.history[version] = rv
	return rv
}

// restore makes a snapshot's rule set live under its original version.
func (s *ruleStore) restore(version uint64, text string, listsJSON []byte, at time.Time, shadows []shadowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, canon, err := s.compile(text, listsJSON, version)
	if err != nil {
		return fmt.Errorf("rules in snapshot (version %d) no longer compile: %w", version, err)
	}
	if err := s.persist(version, text, canon); err != nil {
		return err
	}
	for _, sh := range shadows {
		st := &shadowStat{ID: sh.ID, Text: sh.Text, Action: sh.Action, Since: sh.Since, SinceVersion: sh.SinceVersion, versions: map[uint64]bool{}}
		for _, v := range sh.Versions {
			st.versions[v] = true
		}
		st.evaluated.Store(sh.Evaluated)
		st.matched.Store(sh.Matched)
		s.shadows[sh.ID] = st
	}
	// install treats a shadow rule as continuing only if the previous live
	// version had it; after a restart the previous version is this one.
	s.cur.Store(&ruleVersion{Version: version})
	rv := s.install(version, text, canon, rs, at)
	s.cur.Store(rv)
	return nil
}

// historyFile names version v's rule text in the history directory.
func historyFile(dir string, v uint64) (rulesPath, listsPath string) {
	base := filepath.Join(dir, fmt.Sprintf("v%06d", v))
	return base + ".rules", base + ".lists.json"
}

var historyName = regexp.MustCompile(`^v(\d+)\.rules$`)

// persist writes a version to the history directory, unless it is already
// there with the same content. A version number is never reused for
// different rules.
func (s *ruleStore) persist(v uint64, text string, canon json.RawMessage) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	rp, lp := historyFile(s.dir, v)
	if old, err := os.ReadFile(rp); err == nil {
		if string(old) != text {
			return fmt.Errorf("history %s already holds different rules for version %d", rp, v)
		}
		return nil
	}
	if err := writeFileAtomic(lp, canon); err != nil {
		return err
	}
	return writeFileAtomic(rp, []byte(text))
}

// loadHistory reads every version in the history directory into memory,
// so decisions made before a restart can still be audited and explained.
func (s *ruleStore) loadHistory() error {
	if s.dir == "" {
		return nil
	}
	sets, err := LoadRuleHistory(s.dir, s.cat)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for v, h := range sets {
		if _, ok := s.history[v]; !ok {
			s.history[v] = &ruleVersion{Version: v, Text: h.Text, ListsJSON: h.ListsJSON, Set: h.Set}
		}
	}
	return nil
}

// HistoricalRules is one version read back from a history directory.
type HistoricalRules struct {
	Text      string
	ListsJSON json.RawMessage
	Set       *rules.RuleSet
}

// LoadRuleHistory compiles every version in a rule history directory.
func LoadRuleHistory(dir string, cat *schema.Catalog) (map[uint64]*HistoricalRules, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[uint64]*HistoricalRules{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[uint64]*HistoricalRules{}
	for _, e := range ents {
		m := historyName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return nil, err
		}
		rp, lp := historyFile(dir, v)
		text, err := os.ReadFile(rp)
		if err != nil {
			return nil, err
		}
		listsJSON, err := os.ReadFile(lp)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		var lists rules.Lists
		if len(listsJSON) > 0 && string(listsJSON) != "null" {
			if lists, err = rules.ParseLists(listsJSON); err != nil {
				return nil, fmt.Errorf("%s: %w", lp, err)
			}
		}
		rs, err := rules.Load(string(text), rules.Env{Catalog: cat, Lists: lists}, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rp, err)
		}
		out[v] = &HistoricalRules{Text: string(text), ListsJSON: listsJSON, Set: rs}
	}
	return out, nil
}

// version returns a version from the history.
func (s *ruleStore) version(v uint64) (*ruleVersion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rv, ok := s.history[v]
	return rv, ok
}

// ruleCounts lists every live rule's decision counter, for /metrics.
func (s *ruleStore) ruleCounts() []ruleCount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ruleCount, 0, len(s.counters))
	for id, c := range s.counters {
		out = append(out, ruleCount{id: id, action: s.actions[id], n: c.Load()})
	}
	return out
}

// shadowRecord is a shadowStat in a snapshot or report.
type shadowRecord struct {
	ID           string    `json:"id"`
	Text         string    `json:"rule"`
	Action       string    `json:"action"`
	Since        time.Time `json:"since"`
	SinceVersion uint64    `json:"since_version"`
	Versions     []uint64  `json:"versions"`
	Evaluated    uint64    `json:"evaluated"`
	Matched      uint64    `json:"matched"`
}

// shadowRecords returns the stats of the shadow rules in the live set.
func (s *ruleStore) shadowRecords() []shadowRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.cur.Load()
	var out []shadowRecord
	if cur == nil {
		return out
	}
	for _, st := range cur.live {
		r := shadowRecord{
			ID: st.ID, Text: st.Text, Action: st.Action, Since: st.Since, SinceVersion: st.SinceVersion,
			Evaluated: st.evaluated.Load(), Matched: st.matched.Load(),
		}
		for v := range st.versions {
			r.Versions = append(r.Versions, v)
		}
		slices.Sort(r.Versions)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b shadowRecord) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// canonicalLists re-encodes lists JSON with sorted keys and no spacing, so
// identical lists always persist identically.
func canonicalLists(b []byte) (json.RawMessage, error) {
	var v map[string][]any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v) // encoding/json sorts map keys
}
