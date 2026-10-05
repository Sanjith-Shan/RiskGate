package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sanjith-Shan/RiskGate/internal/stream"
)

// env is one experiment run's configuration and working directory.
type env struct {
	bin     string // directory holding riskgate, serveparity
	brokers string
	prefix  string
	work    string // row-level files and logs; under var/
	model   string
	rules   string
	lists   string
	data    string
	layout  stream.Layout
	session time.Duration
	every   time.Duration
	extra   []string // extra `stream run` flags for every member
}

func (e *env) exe(name string) string {
	p := filepath.Join(e.bin, name)
	if _, err := os.Stat(p + ".exe"); err == nil {
		return p + ".exe"
	}
	return p
}

// run runs a tool to completion and returns its stdout; stderr goes to a
// log file in the work directory.
func (e *env) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.exe(name), args...)
	logf, err := os.OpenFile(filepath.Join(e.work, name+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, logf
	err = cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w (see %s)", name, strings.Join(args, " "), err, logf.Name())
	}
	return out.Bytes(), nil
}

func (e *env) kafkaArgs() []string { return []string{"-brokers", e.brokers, "-prefix", e.prefix} }

// topics creates the run's topics. Every run has its own prefix, so
// nothing is deleted (Kafka on Windows cannot delete a topic without
// failing its log directory).
func (e *env) topics(ctx context.Context) error {
	l := e.layout
	_, err := e.run(ctx, "riskgate", append([]string{"stream", "topics",
		"-payments", strconv.Itoa(int(l.Payments)), "-entity", strconv.Itoa(int(l.EntityEvents)),
		"-parts", strconv.Itoa(int(l.Parts)), "-decisions", strconv.Itoa(int(l.Decisions))}, e.kafkaArgs()...)...)
	return err
}

// member is one pipeline process.
type member struct {
	name     string
	instance string // static membership id; "" for dynamic
	port     int
	stages   string
	restarts int
	cmd      *exec.Cmd
	logPath  string
	started  time.Time
	exited   chan error
}

func (e *env) start(m *member) error {
	args := append([]string{"stream", "run"}, e.kafkaArgs()...)
	args = append(args,
		"-stages", m.stages, "-instance-id", m.instance, "-metrics-addr", "127.0.0.1:"+strconv.Itoa(m.port),
		"-model", e.model, "-rules", e.rules, "-lists", e.lists,
		"-state-dir", filepath.Join(e.work, "state"), "-label-log", filepath.Join(e.work, "labels-"+m.name+".jsonl"),
		"-rules-history", filepath.Join(e.work, "rules-history"),
		"-session-timeout", e.session.String(), "-checkpoint-every", e.every.String(), "-log-level", "info")
	args = append(args, e.extra...)
	m.logPath = filepath.Join(e.work, fmt.Sprintf("%s-%d.log", m.name, m.restarts))
	logf, err := os.Create(m.logPath)
	if err != nil {
		return err
	}
	m.cmd = exec.Command(e.exe("riskgate"), args...)
	m.cmd.Stdout, m.cmd.Stderr = logf, logf
	m.started = time.Now()
	if err := m.cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	m.exited = make(chan error, 1)
	go func() {
		err := m.cmd.Wait()
		logf.Close()
		m.exited <- err
	}()
	// A member that cannot start (no broker, no topics) exits at once.
	select {
	case err := <-m.exited:
		m.exited <- err
		return fmt.Errorf("%s exited at start (%v); see %s", m.name, err, m.logPath)
	case <-time.After(2 * time.Second):
	}
	return nil
}

// kill is SIGKILL (TerminateProcess on Windows): no checkpoint, no commit,
// no leaving the group.
func (m *member) kill() {
	if m.cmd == nil || m.cmd.Process == nil {
		return
	}
	_ = m.cmd.Process.Kill()
	<-m.exited
	m.cmd = nil
}

// quit asks the member to stop cleanly (checkpoint, commit, leave) and
// kills it if it does not within the timeout.
func (m *member) quit(timeout time.Duration) error {
	if m.cmd == nil {
		return nil
	}
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/quit", m.port), "text/plain", nil)
	if err == nil {
		resp.Body.Close()
	}
	select {
	case err := <-m.exited:
		m.cmd = nil
		return err
	case <-time.After(timeout):
		m.kill()
		return fmt.Errorf("%s did not stop within %v; killed", m.name, timeout)
	}
}

func (m *member) metrics() (map[string]stream.StageMetrics, error) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", m.port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]stream.StageMetrics
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// logEvents reads a member's JSON log: per stage, how long after start the
// first batch was processed, and every partition restore's duration.
type logEvents struct {
	FirstBatchMs map[string]int64 `json:"first_batch_ms"`
	RestoreMs    []int64          `json:"restore_ms"`
	Assigned     int              `json:"partitions_assigned"`
	Restored     int              `json:"partitions_restored"`
	Errors       []string         `json:"errors,omitempty"`
}

func readLogEvents(path string) logEvents {
	ev := logEvents{FirstBatchMs: map[string]int64{}}
	f, err := os.Open(path)
	if err != nil {
		return ev
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<22)
	for sc.Scan() {
		var l struct {
			Msg       string `json:"msg"`
			Level     string `json:"level"`
			Stage     string `json:"stage"`
			Since     int64  `json:"since_start_ms"`
			Restored  bool   `json:"restored"`
			RestoreMs int64  `json:"restore_ms"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Msg {
		case "first batch processed":
			ev.FirstBatchMs[l.Stage] = l.Since
		case "partition assigned":
			ev.Assigned++
			if l.Restored {
				ev.Restored++
				ev.RestoreMs = append(ev.RestoreMs, l.RestoreMs)
			}
		}
		if l.Level == "ERROR" && len(ev.Errors) < 5 {
			ev.Errors = append(ev.Errors, l.Msg+": "+l.Error)
		}
	}
	return ev
}

// appendRow appends one JSON line to a results file.
func appendRow(path string, row any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
