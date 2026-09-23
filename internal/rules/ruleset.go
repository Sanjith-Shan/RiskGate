package rules

import (
	"github.com/Sanjith-Shan/RiskGate/internal/schema"
)

// RuleSet is a compiled, immutable set of rules. It is safe for concurrent
// use; the service swaps whole RuleSets atomically rather than editing one.
type RuleSet struct {
	Version  uint64          // assigned by the caller; recorded on every Decision
	Rules    []*CompiledRule // every rule, in source order
	Warnings Diagnostics     // from Lint, when built by Load

	allow, block, review []*CompiledRule // live rules by action, source order
	shadow               []*CompiledRule
}

// Decision is the outcome of evaluating a payment against a RuleSet.
type Decision struct {
	Action Action
	// Rule is the live rule that decided, or nil when no rule matched and
	// the payment is allowed by default.
	Rule    *CompiledRule
	Version uint64
	// Shadow lists every shadow rule whose condition held, in source order,
	// whatever the live decision was. It is nil when none did, so the common
	// case allocates nothing.
	Shadow []*CompiledRule
}

// NewRuleSet groups compiled rules for evaluation.
func NewRuleSet(version uint64, rules []*CompiledRule) *RuleSet {
	s := &RuleSet{Version: version, Rules: rules}
	for _, r := range rules {
		if r.Shadow {
			s.shadow = append(s.shadow, r)
			continue
		}
		switch r.Action {
		case Allow:
			s.allow = append(s.allow, r)
		case Block:
			s.block = append(s.block, r)
		case Review:
			s.review = append(s.review, r)
		}
	}
	return s
}

// Evaluate decides a payment, following the order Stripe documents for
// Radar: allow rules first, and a payment an allow rule matches is not
// evaluated against block or review rules; then block; then review. A
// payment no rule matches is allowed.
//
// Radar leaves rules within one action unordered, because any match of that
// action yields the same outcome. RiskGate evaluates them in source order and
// reports the first that matched, so the reported rule is deterministic.
//
// Shadow rules never influence the decision. All of them are evaluated on
// every payment, and those that matched are listed in Decision.Shadow, so the
// service can log exactly what the rule would have done and compare that
// with the backtest's prediction before the rule goes live. "Matched" means
// the condition held; whether it would also have changed this payment's
// decision (a shadow block under a live allow would not) is the caller's
// call, since it depends on what the shadow rule is meant to replace.
func (s *RuleSet) Evaluate(row schema.Row) Decision {
	d := Decision{Action: Allow, Version: s.Version}
	for _, r := range s.shadow {
		if r.match(row) {
			d.Shadow = append(d.Shadow, r)
		}
	}
	for _, group := range [3][]*CompiledRule{s.allow, s.block, s.review} {
		for _, r := range group {
			if r.match(row) {
				d.Action, d.Rule = r.Action, r
				return d
			}
		}
	}
	return d
}

// Load parses, checks, lints and compiles a rule set in one step. On
// failure the error is a Diagnostics holding every error in the set, plus
// the warnings, in source order. On success the warnings are in
// RuleSet.Warnings.
func Load(src string, env Env, version uint64) (*RuleSet, error) {
	parsed, ds := parseSource(src, &env)
	var checked []*Rule
	for _, r := range parsed {
		errs := checkRule(r, env)
		if len(errs) == 0 {
			checked = append(checked, r)
		}
		ds = append(ds, errs...)
	}
	// Lint whatever checked cleanly, even if other lines failed: the
	// warnings are just as useful while the errors are being fixed.
	ds = append(ds, Lint(checked)...)
	ds.sort()
	if ds.HasErrors() {
		return nil, ds
	}
	compiled := make([]*CompiledRule, len(checked))
	for i, r := range checked {
		cr, err := Compile(r)
		if err != nil {
			return nil, err // unreachable for a rule that passed Check
		}
		compiled[i] = cr
	}
	s := NewRuleSet(version, compiled)
	s.Warnings = ds
	return s, nil
}
