package stream

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/Sanjith-Shan/RiskGate/internal/data"
)

// Layout is the partition count of each topic.
type Layout struct {
	Payments, EntityEvents, Parts, Decisions, Disputes int32
}

// DefaultLayout is what the experiments run with unless they say otherwise.
var DefaultLayout = Layout{Payments: 8, EntityEvents: 8, Parts: 8, Decisions: 8, Disputes: 1}

// CreateTopics creates the pipeline's topics (replication factor 1: the
// experiments run one broker). Decisions are compacted, one per payment id,
// which is also what makes a decision re-sent after a crash harmless to
// anyone reading the topic by key. Topics that exist are left alone.
func CreateTopics(ctx context.Context, cl *kgo.Client, t Topics, l Layout) error {
	adm := kadm.NewClient(cl)
	compact, forever := "compact", "-1"
	// The internal topics are the state's log: a stateful partition rebuilt
	// from the start needs all of it, so retention never trims them. A real
	// deployment would bound them by deleting below the oldest snapshot's
	// offset.
	keep := map[string]*string{"retention.ms": &forever}
	for _, x := range []struct {
		name  string
		parts int32
		conf  map[string]*string
	}{
		{t.Payments, l.Payments, nil},
		{t.EntityEvents, l.EntityEvents, keep},
		{t.Parts, l.Parts, keep},
		{t.Decisions, l.Decisions, map[string]*string{"cleanup.policy": &compact}},
		{t.Disputes, max(l.Disputes, 1), nil},
		{t.DeadLetters, 1, nil},
	} {
		resp, err := adm.CreateTopic(ctx, x.parts, 1, x.conf, x.name)
		if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("stream: create %s: %w", x.name, err)
		}
		if resp.Err != nil && !errors.Is(resp.Err, kerr.TopicAlreadyExists) {
			return fmt.Errorf("stream: create %s: %w", x.name, resp.Err)
		}
	}
	return nil
}

// DeleteTopics deletes the pipeline's topics, for a fresh experiment run.
func DeleteTopics(ctx context.Context, cl *kgo.Client, t Topics) error {
	resps, err := kadm.NewClient(cl).DeleteTopics(ctx, t.Payments, t.EntityEvents, t.Parts, t.Decisions, t.Disputes, t.DeadLetters)
	if err != nil {
		return err
	}
	for _, r := range resps {
		if r.Err != nil && !errors.Is(r.Err, kerr.UnknownTopicOrPartition) {
			return fmt.Errorf("stream: delete %s: %w", r.Topic, r.Err)
		}
	}
	return nil
}

// ReadTopic calls fn on every record in topic up to its end offsets at the
// time of the call, partition by partition in offset order.
func ReadTopic(ctx context.Context, brokers []string, topic string, fn func(*kgo.Record) error) error {
	admin, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return err
	}
	ends, err := kadm.NewClient(admin).ListEndOffsets(ctx, topic)
	admin.Close()
	if err != nil {
		return err
	}
	want := map[int32]int64{}
	parts := map[int32]kgo.Offset{}
	ends.Each(func(o kadm.ListedOffset) {
		if o.Topic == topic && o.Err == nil && o.Offset > 0 {
			want[o.Partition] = o.Offset
			parts[o.Partition] = kgo.NewOffset().AtStart()
		}
	})
	if len(want) == 0 {
		return nil
	}
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{topic: parts}),
		kgo.FetchMaxBytes(64<<20), kgo.FetchMaxPartitionBytes(16<<20))
	if err != nil {
		return err
	}
	defer cl.Close()
	for len(want) > 0 {
		fs := cl.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, fe := range fs.Errors() {
			return fmt.Errorf("stream: read %s/%d: %w", fe.Topic, fe.Partition, fe.Err)
		}
		var ferr error
		fs.EachRecord(func(r *kgo.Record) {
			end, ok := want[r.Partition]
			if ferr != nil || !ok || r.Offset >= end {
				return
			}
			ferr = fn(r)
			if r.Offset+1 >= end {
				delete(want, r.Partition)
			}
		})
		if ferr != nil {
			return ferr
		}
	}
	return nil
}

// DecisionSet is the decisions topic read back and deduplicated by payment
// id.
type DecisionSet struct {
	Lines      map[string][]byte // the first line seen per payment
	created    map[string]int64
	Records    int // lines read
	Duplicates int // lines for a payment already seen
	// Conflicts are duplicates whose decision differs from the first one's,
	// ignoring when it was written (at_ms, latency_us). A crash may re-send a
	// decision; it must be the same decision.
	Conflicts int
	Example   string
}

// ReadDecisions reads and deduplicates the decisions topic.
func ReadDecisions(ctx context.Context, brokers []string, topic string) (*DecisionSet, error) {
	ds := &DecisionSet{Lines: map[string][]byte{}, created: map[string]int64{}}
	err := ReadTopic(ctx, brokers, topic, func(r *kgo.Record) error { return ds.Add(r.Value) })
	return ds, err
}

// Add records one decision line.
func (ds *DecisionSet) Add(line []byte) error {
	ds.Records++
	var head struct {
		PaymentID string `json:"payment_id"`
		Created   int64  `json:"created"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return fmt.Errorf("stream: decision line: %w", err)
	}
	first, seen := ds.Lines[head.PaymentID]
	if !seen {
		ds.Lines[head.PaymentID] = bytes.Clone(line)
		ds.created[head.PaymentID] = head.Created
		return nil
	}
	ds.Duplicates++
	a, err := decisionContent(first)
	if err != nil {
		return err
	}
	b, err := decisionContent(line)
	if err != nil {
		return err
	}
	if a != b {
		ds.Conflicts++
		if ds.Example == "" {
			ds.Example = head.PaymentID
		}
	}
	return nil
}

// decisionContent is a decision line without its wall-clock fields.
func decisionContent(line []byte) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return "", err
	}
	delete(m, "at_ms")
	delete(m, "latency_us")
	b, err := json.Marshal(m) // map keys marshal sorted
	return string(b), err
}

// WriteSorted writes one line per payment in event-time order, (created,
// TransactionID), the order the offline replay and the decision-log
// comparison (cmd/serveparity compare) expect.
func (ds *DecisionSet) WriteSorted(w io.Writer) error {
	ids := make([]string, 0, len(ds.Lines))
	for id := range ds.Lines {
		ids = append(ids, id)
	}
	txnID := func(id string) int64 {
		n, err := strconv.ParseInt(strings.TrimPrefix(id, data.PaymentIDPrefix), 10, 64)
		if err != nil {
			return -1
		}
		return n
	}
	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Or(cmp.Compare(ds.created[a], ds.created[b]), cmp.Compare(txnID(a), txnID(b)), cmp.Compare(a, b))
	})
	bw := bufio.NewWriterSize(w, 1<<20)
	for _, id := range ids {
		line := ds.Lines[id]
		if _, err := bw.Write(line); err != nil {
			return err
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			if err := bw.WriteByte('\n'); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// WaitForDecisions polls the decisions topic's end offsets until they stop
// growing for quiet, or until want records exist, or ctx ends.
func WaitForDecisions(ctx context.Context, brokers []string, topic string, want int64, quiet time.Duration) (int64, error) {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return 0, err
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)
	var last int64 = -1
	lastChange := time.Now()
	for {
		ends, err := adm.ListEndOffsets(ctx, topic)
		if err != nil {
			return last, err
		}
		var total int64
		ends.Each(func(o kadm.ListedOffset) { total += o.Offset })
		if total != last {
			last, lastChange = total, time.Now()
		}
		if want > 0 && total >= want {
			return total, nil
		}
		if quiet > 0 && time.Since(lastChange) > quiet {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
