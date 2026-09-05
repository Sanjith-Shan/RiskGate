package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// Snapshots.
//
// Everything the service would lose on restart goes into one file: the
// velocity state, the idempotency store, the webhook deduper, the labels,
// and the live rule set with its shadow-rule counters. One file, written to
// a temporary name, fsynced, renamed over the old one, and the directory
// fsynced, so a crash at any point leaves either the old snapshot or the new
// one, never a mixture.
//
// Layout:
//
//	"RGSNAP01\n"
//	uvarint n, n bytes of JSON: snapshotMeta
//	the velocity state (features.State.Snapshot) up to the checksum
//	uint32 CRC-32 (IEEE) of everything above, little-endian
//
// Consistency: a snapshot taken at shutdown, after the HTTP server has
// drained, is a single point in time, and that is what the restart tests
// check. Periodic snapshots run under live traffic: each part is internally
// consistent (each state shard is locked while it is written), but a
// request in flight may be in the velocity state and not yet in the
// idempotency store, or the reverse. Making them agree would take a global
// lock on the payment path, which is exactly what sharding exists to avoid.
// The cost is bounded by the requests in flight at the instant of a crash,
// and those were never acknowledged, so Clearinghouse retries them anyway.

const snapshotMagic = "RGSNAP01\n"

type snapshotMeta struct {
	WrittenAt time.Time `json:"written_at"`
	Catalog   string    `json:"catalog"`
	State     string    `json:"state"`
	Rules     struct {
		Version    uint64          `json:"version"`
		Text       string          `json:"text"`
		Lists      json.RawMessage `json:"lists"`
		DeployedAt time.Time       `json:"deployed_at"`
		Shadows    []shadowRecord  `json:"shadows"`
	} `json:"rules"`
	Idempotency []idemRecord        `json:"idempotency"`
	Dedupe      []webhook.SeenEvent `json:"dedupe"`
	Labels      labelSnapshot       `json:"labels"`
}

// Snapshot writes the snapshot file. It is safe to call under traffic (see
// above) and serialized with other snapshots.
func (s *Service) Snapshot() error {
	if s.cfg.SnapshotPath == "" {
		return errors.New("service: no snapshot path configured")
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	err := s.writeSnapshot(s.cfg.SnapshotPath)
	if err != nil {
		s.metrics.snapshotFailures.Add(1)
		return fmt.Errorf("service: snapshot: %w", err)
	}
	s.lastSnapshot.Store(s.now().UnixNano())
	return nil
}

func (s *Service) writeSnapshot(path string) error {
	var meta snapshotMeta
	meta.WrittenAt = s.now().UTC()
	meta.Catalog = catalogStamp(s.cat)
	meta.State = s.cfg.StateDesc
	cur := s.rules.current()
	meta.Rules.Version, meta.Rules.Text, meta.Rules.Lists, meta.Rules.DeployedAt = cur.Version, cur.Text, cur.ListsJSON, cur.DeployedAt
	meta.Rules.Shadows = s.rules.shadowRecords()
	meta.Idempotency = s.idem.snapshot()
	meta.Dedupe = s.dedupe.Snapshot()
	meta.Labels = s.labels.snapshot()
	metaJSON, err := json.Marshal(&meta)
	if err != nil {
		return err
	}

	return writeAtomic(path, func(w io.Writer) error {
		crc := crc32.NewIEEE()
		mw := io.MultiWriter(w, crc)
		if _, err := io.WriteString(mw, snapshotMagic); err != nil {
			return err
		}
		if _, err := mw.Write(binary.AppendUvarint(nil, uint64(len(metaJSON)))); err != nil {
			return err
		}
		if _, err := mw.Write(metaJSON); err != nil {
			return err
		}
		if err := s.engine.State().Snapshot(mw); err != nil {
			return err
		}
		_, err := w.Write(binary.LittleEndian.AppendUint32(nil, crc.Sum32()))
		return err
	})
}

// restoreSnapshot loads the snapshot file if there is one. It reports
// whether it restored anything. A snapshot that does not match the
// configuration (another catalog or state kind) is an error, not something
// to skip: starting empty would silently reset every velocity feature.
func (s *Service) restoreSnapshot() (bool, error) {
	path := s.cfg.SnapshotPath
	if path == "" {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fail := func(format string, args ...any) (bool, error) {
		return false, fmt.Errorf("service: snapshot %s: %s", path, fmt.Sprintf(format, args...))
	}
	if len(b) < len(snapshotMagic)+4 || string(b[:len(snapshotMagic)]) != snapshotMagic {
		return fail("not a RiskGate snapshot")
	}
	body, sum := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return fail("checksum mismatch (truncated or corrupt)")
	}
	rest := body[len(snapshotMagic):]
	n, k := binary.Uvarint(rest)
	if k <= 0 || n > uint64(len(rest)-k) {
		return fail("bad header")
	}
	var meta snapshotMeta
	if err := json.Unmarshal(rest[k:k+int(n)], &meta); err != nil {
		return fail("%v", err)
	}
	velocity := rest[k+int(n):]
	if meta.Catalog != catalogStamp(s.cat) {
		return fail("written for a different field catalog; move it aside to start fresh")
	}
	if meta.State != s.cfg.StateDesc {
		return fail("holds %q velocity state but the service is configured for %q; restart with the same -state, -sharding and -shards, or move the snapshot aside", meta.State, s.cfg.StateDesc)
	}
	if err := s.engine.State().Restore(bytes.NewReader(velocity)); err != nil {
		return fail("velocity state: %v", err)
	}
	if err := s.rules.restore(meta.Rules.Version, meta.Rules.Text, meta.Rules.Lists, meta.Rules.DeployedAt, meta.Rules.Shadows); err != nil {
		return false, err
	}
	s.idem.restore(meta.Idempotency)
	s.dedupe.Restore(meta.Dedupe)
	s.labels.restore(meta.Labels)
	s.lastSnapshot.Store(meta.WrittenAt.UnixNano())
	s.log.Info("restored snapshot", "path", path, "written_at", meta.WrittenAt, "ruleset_version", meta.Rules.Version,
		"idempotency_entries", len(meta.Idempotency), "dedupe_entries", len(meta.Dedupe), "labels", len(meta.Labels.Payments))
	return true, nil
}

// RunSnapshots snapshots every interval until ctx ends. Failures are
// logged and counted; the service keeps running on its last good snapshot.
func (s *Service) RunSnapshots(ctx context.Context, interval time.Duration) {
	if s.cfg.SnapshotPath == "" || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			start := time.Now()
			if err := s.Snapshot(); err != nil {
				s.log.Error("snapshot failed", "error", err)
				continue
			}
			s.log.Debug("snapshot written", "took", time.Since(start))
		}
	}
}

// writeAtomic writes a file so that readers see either its old contents or
// its new contents in full: write a temporary file in the same directory,
// fsync it, rename it over the target, fsync the directory so the rename
// itself is durable.
func writeAtomic(path string, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	bw := bufio.NewWriterSize(f, 1<<20)
	if err = write(bw); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func writeFileAtomic(path string, b []byte) error {
	return writeAtomic(path, func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}
