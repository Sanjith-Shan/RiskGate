package stream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"

	"github.com/Sanjith-Shan/RiskGate/internal/schema"
	"github.com/Sanjith-Shan/RiskGate/internal/service"
	"github.com/Sanjith-Shan/RiskGate/internal/webhook"
)

// The stages. Each is one consumer group; a process runs any of them.
const (
	StageRoute     = "route"
	StageAggregate = "aggregate"
	StageJoin      = "join"
	StageLabels    = "labels"
)

// Stages lists every stage, in pipeline order.
var Stages = []string{StageRoute, StageAggregate, StageJoin, StageLabels}

// output topics, by role; the runner maps them to names.
const (
	topicEntityEvents = iota
	topicParts
	topicDecisions
	topicDeadLetters
)

// output is a record a task wants written.
type output struct {
	topic     int
	partition int32 // -1: the runner picks (dead letters go to partition 0)
	key       string
	value     []byte
	err       string // dead letters: why
}

// task is one input partition's processing. A runner's tasks run on one
// goroutine each per batch, so a task needs no locking of its own.
type task interface {
	handle(rec in, out []output) ([]output, error)
	// endBatch appends what must follow the batch's output (the router's
	// watermarks); the runner sends it only after the batch is acknowledged.
	endBatch(out []output) []output
	// snapshot is nil, nil for a task with no state to keep.
	snapshot() ([]byte, error)
	restore(b []byte) error
}

// Config configures a Runner.
type Config struct {
	Brokers []string
	Topics  Topics
	Stage   string
	// InstanceID turns on static group membership: a process that restarts
	// with the same id within SessionTimeout gets its partitions back
	// without a rebalance.
	InstanceID     string
	SessionTimeout time.Duration
	// Store keeps the stateful stages' snapshots (aggregate, join).
	Store Store
	// CheckpointEvery is how often a stateful stage snapshots and commits.
	CheckpointEvery time.Duration
	MaxPollRecords  int

	Catalog *schema.Catalog
	Decider *Decider            // join
	Labels  *service.LabelStore // labels
	// ArrivalOrder makes aggregators apply events in arrival order with no
	// watermarks: the negative control for the merger.
	ArrivalOrder bool

	Logger  *slog.Logger
	Metrics *Metrics
}

// Runner runs one stage in one process.
type Runner struct {
	cfg      Config
	cl       *kgo.Client
	group    string
	input    string
	stateful bool
	counts   PartitionCounts
	log      *slog.Logger
	m        *Metrics

	// tasks is touched by the poll loop and the group callbacks, which
	// BlockRebalanceOnPoll serializes, and by shutdown, which runs outside
	// the poll gate; mu covers all three.
	mu      sync.Mutex
	tasks   map[int32]*taskState
	prodErr atomic.Pointer[error]
	started time.Time
	first   sync.Once

	lastCheckpoint time.Time
	crashed        atomic.Bool
	closeOnce      sync.Once
}

type taskState struct {
	t        task
	next     int64 // next input offset to process; -1 until the first record
	restored bool
	dirty    bool // processed records since the last checkpoint
}

// PartitionCounts are the pipeline topics' partition counts, read from the
// cluster at start. Changing one under a running pipeline would move keys
// between partitions, so the snapshots record what they were built with.
type PartitionCounts struct {
	Payments, EntityEvents, Parts, Decisions int32
}

// ReadPartitionCounts asks the cluster.
func ReadPartitionCounts(ctx context.Context, cl *kgo.Client, t Topics) (PartitionCounts, error) {
	td, err := kadm.NewClient(cl).ListTopics(ctx, t.Payments, t.EntityEvents, t.Parts, t.Decisions)
	if err != nil {
		return PartitionCounts{}, err
	}
	get := func(name string) (int32, error) {
		d, ok := td[name]
		if !ok || d.Err != nil || len(d.Partitions) == 0 {
			return 0, fmt.Errorf("stream: topic %s is missing (create the topics first): %v", name, d.Err)
		}
		return int32(len(d.Partitions)), nil
	}
	var c PartitionCounts
	for _, x := range []struct {
		dst  *int32
		name string
	}{{&c.Payments, t.Payments}, {&c.EntityEvents, t.EntityEvents}, {&c.Parts, t.Parts}, {&c.Decisions, t.Decisions}} {
		if *x.dst, err = get(x.name); err != nil {
			return c, err
		}
	}
	return c, nil
}

// NewRunner connects and joins the stage's consumer group.
func NewRunner(ctx context.Context, cfg Config) (*Runner, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = &Metrics{}
	}
	if cfg.CheckpointEvery <= 0 {
		cfg.CheckpointEvery = 5 * time.Second
	}
	if cfg.MaxPollRecords <= 0 {
		cfg.MaxPollRecords = 4096
	}
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = 10 * time.Second
	}
	if cfg.Catalog == nil {
		cfg.Catalog = schema.Default()
	}
	r := &Runner{cfg: cfg, tasks: map[int32]*taskState{}, log: cfg.Logger.With("stage", cfg.Stage), m: cfg.Metrics, started: time.Now()}
	switch cfg.Stage {
	case StageRoute:
		r.input = cfg.Topics.Payments
	case StageAggregate:
		r.input, r.stateful = cfg.Topics.EntityEvents, true
	case StageJoin:
		r.input, r.stateful = cfg.Topics.Parts, true
		if cfg.Decider == nil {
			return nil, errors.New("stream: the join stage needs a Decider")
		}
	case StageLabels:
		r.input = cfg.Topics.Disputes
		if cfg.Labels == nil {
			return nil, errors.New("stream: the labels stage needs a label store")
		}
	default:
		return nil, fmt.Errorf("stream: unknown stage %q (want one of %v)", cfg.Stage, Stages)
	}
	if r.stateful && cfg.Store == nil {
		return nil, fmt.Errorf("stream: the %s stage needs a snapshot store", cfg.Stage)
	}
	r.group = cfg.Topics.group(cfg.Stage)

	admin, err := kgo.NewClient(kgo.SeedBrokers(cfg.Brokers...))
	if err != nil {
		return nil, err
	}
	r.counts, err = ReadPartitionCounts(ctx, admin, cfg.Topics)
	admin.Close()
	if err != nil {
		return nil, err
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ClientID("riskgate-" + cfg.Stage),
		kgo.ConsumerGroup(r.group),
		kgo.ConsumeTopics(r.input),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.SessionTimeout(cfg.SessionTimeout),
		kgo.HeartbeatInterval(cfg.SessionTimeout / 4),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.OnPartitionsAssigned(r.assigned),
		kgo.OnPartitionsRevoked(r.revoked),
		kgo.OnPartitionsLost(r.lost),
		kgo.AdjustFetchOffsetsFn(r.adjust),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// A lost batch followed by acknowledged ones would leave a gap the
		// high-water-mark dedupe downstream makes permanent: stop instead.
		kgo.StopProducerOnDataLossDetected(),
		kgo.ProducerLinger(2 * time.Millisecond),
		kgo.MaxBufferedRecords(1 << 20),
		kgo.FetchMaxWait(100 * time.Millisecond),
	}
	if cfg.InstanceID != "" {
		opts = append(opts, kgo.InstanceID(cfg.InstanceID))
	}
	if r.cl, err = kgo.NewClient(opts...); err != nil {
		return nil, err
	}
	return r, nil
}

// newTask builds a fresh task for an input partition.
func (r *Runner) newTask(p int32) (task, error) {
	switch r.cfg.Stage {
	case StageRoute:
		return &routeTask{newRouter(p, r.counts.EntityEvents, r.counts.Parts), r.m}, nil
	case StageAggregate:
		a, err := newAggregator(r.cfg.Catalog, p, r.counts.Payments, r.counts.Parts)
		if err != nil {
			return nil, err
		}
		a.arrival = r.cfg.ArrivalOrder
		return &aggregateTask{a, r.m}, nil
	case StageJoin:
		return &joinTask{newJoiner(r.cfg.Decider, p, r.counts.Decisions), r.m}, nil
	default:
		return &labelTask{r.cfg.Labels}, nil
	}
}

// Run polls and processes until ctx is cancelled or a fatal error. On
// cancellation it checkpoints every task and leaves the group cleanly.
func (r *Runner) Run(ctx context.Context) error {
	defer r.close()
	for {
		fs := r.cl.PollRecords(ctx, r.cfg.MaxPollRecords)
		if r.crashed.Load() {
			return errCrashed
		}
		if fs.IsClientClosed() || ctx.Err() != nil {
			r.cl.AllowRebalance()
			return r.shutdown()
		}
		for _, fe := range fs.Errors() {
			if errors.Is(fe.Err, context.Canceled) {
				continue
			}
			r.log.Warn("fetch error", "topic", fe.Topic, "partition", fe.Partition, "error", fe.Err)
		}
		// A polled batch is finished even if shutdown was asked for meanwhile:
		// its output is flushed and committed, then shutdown checkpoints.
		if err := r.batch(context.WithoutCancel(ctx), fs); err != nil {
			// Tasks may have advanced past output that was never sent: a
			// checkpoint now would save that state and lose the output. Exit
			// as a crash does and let the restart resume from the last
			// good snapshot.
			r.crashed.Store(true)
			r.cl.AllowRebalance()
			return err
		}
		r.cl.AllowRebalance()
	}
}

// batch processes one poll's records, each partition on its own goroutine,
// then writes the output, then commits or checkpoints.
func (r *Runner) batch(ctx context.Context, fs kgo.Fetches) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	type work struct {
		p    int32
		ts   *taskState
		recs []*kgo.Record
		out  []output
		err  error
	}
	var ws []*work
	fs.EachPartition(func(ftp kgo.FetchTopicPartition) {
		if len(ftp.Records) == 0 {
			return
		}
		ts := r.tasks[ftp.Partition]
		if ts == nil {
			r.log.Warn("records for a partition with no task", "partition", ftp.Partition)
			return
		}
		ws = append(ws, &work{p: ftp.Partition, ts: ts, recs: ftp.Records})
	})
	if len(ws) == 0 {
		return r.maybeCheckpoint(ctx, false)
	}
	start := time.Now()
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, rec := range w.recs {
				if rec.Offset < w.ts.next {
					continue // already processed: a refetch after a seek
				}
				if r.stateful && w.ts.next >= 0 && rec.Offset != w.ts.next {
					// The input topics are neither compacted nor transactional,
					// so offsets are contiguous: a gap is records deleted by
					// retention or a topic recreated under old snapshots, and
					// state rebuilt over it would be wrong.
					w.err = fmt.Errorf("partition %d: expected offset %d, got %d (input deleted or topic recreated?)", w.p, w.ts.next, rec.Offset)
					return
				}
				w.out, w.err = w.ts.t.handle(inOf(rec), w.out)
				if w.err != nil {
					w.err = fmt.Errorf("partition %d offset %d: %w", w.p, rec.Offset, w.err)
					return
				}
				w.ts.next = rec.Offset + 1
				w.ts.dirty = true
			}
		}()
	}
	wg.Wait()
	var n int
	for _, w := range ws {
		if w.err != nil {
			return w.err
		}
		n += len(w.recs)
		r.produce(ctx, w.out)
	}
	r.m.add(r.cfg.Stage, func(s *StageMetrics) {
		s.Records += uint64(n)
		s.Batches++
		s.BusyNanos += uint64(time.Since(start))
	})
	r.first.Do(func() {
		r.log.Info("first batch processed", "since_start_ms", time.Since(r.started).Milliseconds(), "records", n)
		r.m.add(r.cfg.Stage, func(s *StageMetrics) { s.FirstBatchMs = time.Since(r.started).Milliseconds() })
	})

	if r.stateful {
		return r.maybeCheckpoint(ctx, false)
	}
	// Stateless stages: once the output is acknowledged, send what must
	// follow it, and commit.
	if err := r.flush(ctx); err != nil {
		return err
	}
	var tail []output
	for _, w := range ws {
		tail = w.ts.t.endBatch(tail)
	}
	if len(tail) > 0 {
		r.produce(ctx, tail)
		if err := r.flush(ctx); err != nil {
			return err
		}
	}
	commits := map[int32]int64{}
	for _, w := range ws {
		commits[w.p] = w.ts.next
		w.ts.dirty = false
	}
	return r.commit(ctx, commits)
}

func inOf(rec *kgo.Record) in {
	x := in{offset: rec.Offset, key: rec.Key, value: rec.Value, timestamp: rec.Timestamp.UnixMilli()}
	for _, h := range rec.Headers {
		if h.Key == HeaderKind && string(h.Value) == KindHeartbeat {
			x.heartbeat = true
		}
	}
	return x
}

func (r *Runner) topicName(role int) string {
	switch role {
	case topicEntityEvents:
		return r.cfg.Topics.EntityEvents
	case topicParts:
		return r.cfg.Topics.Parts
	case topicDecisions:
		return r.cfg.Topics.Decisions
	default:
		return r.cfg.Topics.DeadLetters
	}
}

// produce hands records to the client, in order. Errors surface at flush.
func (r *Runner) produce(ctx context.Context, outs []output) {
	for i := range outs {
		o := &outs[i]
		rec := &kgo.Record{Topic: r.topicName(o.topic), Partition: o.partition, Key: []byte(o.key), Value: o.value}
		if o.partition < 0 {
			rec.Partition = 0
		}
		if o.err != "" {
			rec.Headers = []kgo.RecordHeader{{Key: "riskgate-error", Value: []byte(o.err)}}
		}
		r.cl.Produce(ctx, rec, func(_ *kgo.Record, err error) {
			if err != nil {
				r.prodErr.CompareAndSwap(nil, &err)
			}
		})
	}
	r.m.add(r.cfg.Stage, func(s *StageMetrics) { s.Outputs += uint64(len(outs)) })
}

// flush waits until everything produced so far is acknowledged.
func (r *Runner) flush(ctx context.Context) error {
	if err := r.cl.Flush(ctx); err != nil {
		return err
	}
	if p := r.prodErr.Load(); p != nil {
		return fmt.Errorf("stream: produce: %w", *p)
	}
	return nil
}

func (r *Runner) commit(ctx context.Context, next map[int32]int64) error {
	if r.crashed.Load() {
		return errCrashed
	}
	if len(next) == 0 {
		return nil
	}
	offsets := map[string]map[int32]kgo.EpochOffset{r.input: {}}
	for p, o := range next {
		if o >= 0 {
			offsets[r.input][p] = kgo.EpochOffset{Epoch: -1, Offset: o}
		}
	}
	var cerr error
	r.cl.CommitOffsetsSync(ctx, offsets, func(_ *kgo.Client, _ *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
		if err != nil {
			cerr = err
			return
		}
		for _, t := range resp.Topics {
			for _, p := range t.Partitions {
				if p.ErrorCode != 0 && cerr == nil {
					cerr = fmt.Errorf("stream: commit %s/%d: error code %d", t.Topic, p.Partition, p.ErrorCode)
				}
			}
		}
	})
	return cerr
}

// maybeCheckpoint snapshots and commits every task if CheckpointEvery has
// passed (or force). The order is what makes a crash safe:
//
//  1. flush: every output of the processed input is acknowledged;
//  2. snapshot each task with the offset of its next input record;
//  3. commit those offsets.
//
// A crash before 2 restarts from the previous snapshot and re-sends output
// the joiner and the decision readers already have; they drop it by
// sequence number and payment id. A crash between 2 and 3 restarts from the
// new snapshot, whose offset, not the group's, is where a restored task
// resumes, so committed offsets only ever trail the state.
func (r *Runner) maybeCheckpoint(ctx context.Context, force bool) error {
	if !r.stateful {
		return nil
	}
	if !force && time.Since(r.lastCheckpoint) < r.cfg.CheckpointEvery {
		return nil
	}
	r.lastCheckpoint = time.Now()
	parts := make([]int32, 0, len(r.tasks))
	for p, ts := range r.tasks {
		if ts.dirty {
			parts = append(parts, p)
		}
	}
	return r.checkpoint(ctx, parts)
}

func (r *Runner) checkpoint(ctx context.Context, parts []int32) error {
	if r.crashed.Load() {
		return errCrashed
	}
	if len(parts) == 0 {
		return nil
	}
	start := time.Now()
	if err := r.flush(ctx); err != nil {
		return err
	}
	commits := map[int32]int64{}
	var bytes int
	for _, p := range parts {
		ts := r.tasks[p]
		if ts == nil || ts.next < 0 {
			continue
		}
		b, err := ts.t.snapshot()
		if err != nil {
			return fmt.Errorf("stream: snapshot partition %d: %w", p, err)
		}
		if err := r.cfg.Store.Put(r.group, p, ts.next, b); err != nil {
			return fmt.Errorf("stream: save snapshot partition %d: %w", p, err)
		}
		bytes += len(b)
		ts.dirty = false
		commits[p] = ts.next
	}
	err := r.commit(ctx, commits)
	r.m.add(r.cfg.Stage, func(s *StageMetrics) {
		s.Checkpoints++
		s.CheckpointNanos += uint64(time.Since(start))
		s.SnapshotBytes = uint64(bytes)
	})
	return err
}

var errCrashed = errors.New("stream: crashed on purpose")

// Crash stops the runner the way a killed process stops: no checkpoint, no
// commit, no leaving the group, output in flight abandoned. Tests use it;
// the crash experiment kills real processes instead.
func (r *Runner) Crash() {
	r.crashed.Store(true)
	go r.close()
}

// close closes the client once: concurrent Closes race inside the client.
func (r *Runner) close() { r.closeOnce.Do(r.cl.Close) }

// shutdown checkpoints everything and leaves.
func (r *Runner) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var err error
	r.mu.Lock()
	if r.stateful {
		parts := make([]int32, 0, len(r.tasks))
		for p := range r.tasks {
			parts = append(parts, p)
		}
		err = r.checkpoint(ctx, parts)
	} else {
		err = r.flush(ctx)
	}
	r.mu.Unlock() // leaving runs the revoke callback, which takes mu
	if lerr := r.cl.LeaveGroupContext(ctx); lerr != nil && err == nil && r.cfg.InstanceID == "" {
		err = lerr
	}
	return err
}

// Group callbacks. BlockRebalanceOnPoll runs them between batches, never
// during one.

func (r *Runner) assigned(ctx context.Context, _ *kgo.Client, assigned map[string][]int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range assigned[r.input] {
		start := time.Now()
		t, err := r.newTask(p)
		if err != nil {
			r.log.Error("cannot build task", "partition", p, "error", err)
			continue
		}
		ts := &taskState{t: t, next: -1}
		if r.stateful {
			off, b, ok, err := r.cfg.Store.Get(r.group, p)
			if err == nil && ok {
				if err = t.restore(b); err == nil {
					ts.next, ts.restored = off, true
				}
			}
			if err != nil {
				// Starting the partition over with empty state would be
				// silently wrong once retention has trimmed its input, so
				// an unreadable snapshot stops the process.
				r.log.Error("cannot restore snapshot; stopping", "partition", p, "error", err)
				r.crashed.Store(true)
				go r.close()
				return
			}
		}
		r.tasks[p] = ts
		el := time.Since(start)
		r.m.add(r.cfg.Stage, func(s *StageMetrics) {
			s.Assigned++
			if ts.restored {
				s.Restores++
				s.RestoreNanos += uint64(el)
			}
		})
		r.log.Info("partition assigned", "partition", p, "restored", ts.restored, "offset", ts.next, "restore_ms", el.Milliseconds())
	}
}

// adjust points each restored partition at its snapshot's offset. A
// stateful partition without a snapshot starts at the beginning whatever
// the group committed, because its state starts empty.
func (r *Runner) adjust(_ context.Context, offsets map[string]map[int32]kgo.Offset) (map[string]map[int32]kgo.Offset, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := range offsets[r.input] {
		ts := r.tasks[p]
		if ts == nil || !r.stateful {
			continue
		}
		if ts.restored {
			offsets[r.input][p] = kgo.NewOffset().At(ts.next).WithEpoch(-1)
		} else {
			offsets[r.input][p] = kgo.NewOffset().AtStart()
		}
	}
	return offsets, nil
}

func (r *Runner) revoked(ctx context.Context, _ *kgo.Client, revoked map[string][]int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	parts := revoked[r.input]
	if len(parts) == 0 || r.crashed.Load() {
		return
	}
	var err error
	if r.stateful {
		err = r.checkpoint(ctx, parts)
	} else {
		err = r.flush(ctx)
		if err == nil {
			commits := map[int32]int64{}
			for _, p := range parts {
				if ts := r.tasks[p]; ts != nil && ts.dirty {
					commits[p] = ts.next
				}
			}
			err = r.commit(ctx, commits)
		}
	}
	if err != nil {
		r.log.Error("handing over revoked partitions failed; the next owner resumes from the last snapshot", "partitions", parts, "error", err)
	}
	for _, p := range parts {
		delete(r.tasks, p)
	}
	r.m.add(r.cfg.Stage, func(s *StageMetrics) { s.Revoked += uint64(len(parts)) })
	r.log.Info("partitions revoked", "partitions", parts)
}

func (r *Runner) lost(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.crashed.Load() {
		return
	}
	for _, p := range lost[r.input] {
		delete(r.tasks, p)
	}
	r.m.add(r.cfg.Stage, func(s *StageMetrics) { s.Lost += uint64(len(lost[r.input])) })
	r.log.Warn("partitions lost; the next owner resumes from the last snapshot", "partitions", lost[r.input])
}

// Task adapters.

type routeTask struct {
	r *router
	m *Metrics
}

func (t *routeTask) handle(rec in, out []output) ([]output, error) {
	before := *t.r
	out, err := t.r.handle(rec, out)
	t.m.add(StageRoute, func(s *StageMetrics) {
		s.Payments += t.r.payments - before.payments
		s.Heartbeats += t.r.heartbeats - before.heartbeats
		s.DeadLetters += t.r.deadLetters - before.deadLetters
		s.OrderViolations += t.r.orderViolations - before.orderViolations
	})
	return out, err
}
func (t *routeTask) endBatch(out []output) []output { return t.r.watermarks(out) }
func (t *routeTask) snapshot() ([]byte, error)      { return nil, nil }
func (t *routeTask) restore([]byte) error           { return nil }

type aggregateTask struct {
	a *aggregator
	m *Metrics
}

func (t *aggregateTask) handle(rec in, out []output) ([]output, error) {
	dups, late := t.a.merge.dups, t.a.merge.late
	out, err := t.a.handle(rec.value, out)
	if d, l := t.a.merge.dups-dups, t.a.merge.late-late; d+l > 0 {
		t.m.add(StageAggregate, func(s *StageMetrics) { s.Duplicates += d; s.Late += l })
	}
	return out, err
}
func (t *aggregateTask) endBatch(out []output) []output { return out }
func (t *aggregateTask) snapshot() ([]byte, error)      { return t.a.snapshot() }
func (t *aggregateTask) restore(b []byte) error         { return t.a.restore(b) }

type joinTask struct {
	j *joiner
	m *Metrics
}

func (t *joinTask) handle(rec in, out []output) ([]output, error) {
	dups, decided := t.j.dups, t.j.decided
	out, err := t.j.handle(rec.value, out)
	if d, n := t.j.dups-dups, t.j.decided-decided; d+n > 0 {
		t.m.add(StageJoin, func(s *StageMetrics) { s.Duplicates += d; s.Decided += n })
	}
	return out, err
}
func (t *joinTask) endBatch(out []output) []output { return out }
func (t *joinTask) snapshot() ([]byte, error)      { return t.j.snapshot() }
func (t *joinTask) restore(b []byte) error         { return t.j.restore(b) }

// labelTask records dispute events as labels. The label store appends and
// fsyncs before Record returns, and upserts by dispute id, so a redelivered
// event changes nothing: the offset is committed after the label is durable.
type labelTask struct{ labels *service.LabelStore }

func (t *labelTask) handle(rec in, out []output) ([]output, error) {
	e, err := webhook.ParseEvent(rec.value)
	if err != nil {
		return append(out, output{topic: topicDeadLetters, partition: -1, key: string(rec.key), value: rec.value, err: err.Error()}), nil
	}
	return out, t.labels.Record(e)
}
func (t *labelTask) endBatch(out []output) []output { return out }
func (t *labelTask) snapshot() ([]byte, error)      { return nil, nil }
func (t *labelTask) restore([]byte) error           { return nil }
