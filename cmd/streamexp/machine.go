package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Provenance is stamped on every result row: what ran, where, and how busy
// the machine was, because a throughput number from a busy machine is not
// a throughput number.
type Provenance struct {
	DateUTC    string  `json:"date_utc"`
	GitCommit  string  `json:"git_commit"`
	GitDirty   bool    `json:"git_dirty"`
	GoVersion  string  `json:"go_version"`
	OS         string  `json:"os"`
	CPU        string  `json:"cpu"`
	LogicalCPU int     `json:"logical_cpus"`
	MemGB      float64 `json:"mem_gb"`
	Kafka      string  `json:"kafka"`
	Load       Load    `json:"load"`
}

// Load describes what else the machine was doing.
type Load struct {
	// Whole-machine CPU busy share in the 10 s before the run started, with
	// nothing of this experiment running: the background load.
	BaselineCPUPct float64 `json:"baseline_cpu_pct"`
	// Whole-machine CPU busy share sampled every second during the run,
	// this experiment's processes included.
	RunCPUPctMean float64 `json:"run_cpu_pct_mean"`
	RunCPUPctMax  float64 `json:"run_cpu_pct_max"`
	// Other sessions on the machine: a benchmark lock held by another
	// project, and the 1-minute load average inside WSL, where they run.
	ForeignBenchLock string `json:"foreign_bench_lock,omitempty"`
	WSLLoadAvg       string `json:"wsl_loadavg,omitempty"`
	OtherContainers  int    `json:"other_docker_containers"`
	Quiet            bool   `json:"quiet"`
}

func sh(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(out), "\x00", ""))
}

func provenance() Provenance {
	p := Provenance{
		DateUTC: time.Now().UTC().Format(time.RFC3339), GitCommit: sh("git", "rev-parse", "HEAD"),
		GitDirty:  sh("git", "status", "--porcelain", "--untracked-files=no", "--", "cmd", "internal", "go.mod") != "",
		GoVersion: runtime.Version(), OS: runtime.GOOS + "/" + runtime.GOARCH, LogicalCPU: runtime.NumCPU(),
		Kafka: "Apache Kafka 3.9.1, one broker (KRaft), localhost",
	}
	p.CPU, p.MemGB = cpuAndMemory()
	return p
}

// foreignLoad reads the state of the other sessions sharing the machine.
func foreignLoad(l *Load) {
	wsl := func(args ...string) string {
		if runtime.GOOS != "windows" {
			return sh(args[0], args[1:]...)
		}
		return sh("wsl.exe", append([]string{"-d", "Ubuntu-24.04", "--"}, args...)...)
	}
	lock := wsl("cat", "/tmp/BENCH_LOCK")
	if lock != "" && !strings.HasPrefix(lock, "riskgate") {
		l.ForeignBenchLock = lock
	}
	l.WSLLoadAvg = wsl("cat", "/proc/loadavg")
	if ps := wsl("docker", "ps", "-q"); ps != "" {
		l.OtherContainers = len(strings.Fields(ps))
	}
}

// cpuSampler measures whole-machine CPU busy share once a second.
type cpuSampler struct {
	mu      sync.Mutex
	samples []float64
	stop    chan struct{}
	done    chan struct{}
}

func startSampler() *cpuSampler {
	s := &cpuSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		prevIdle, prevTotal := cpuTimes()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				idle, total := cpuTimes()
				if dt := total - prevTotal; dt > 0 {
					s.mu.Lock()
					s.samples = append(s.samples, 100*(1-float64(idle-prevIdle)/float64(dt)))
					s.mu.Unlock()
				}
				prevIdle, prevTotal = idle, total
			}
		}
	}()
	return s
}

func (s *cpuSampler) finish() (mean, maxv float64) {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.samples {
		mean += x
		maxv = max(maxv, x)
	}
	if len(s.samples) > 0 {
		mean /= float64(len(s.samples))
	}
	return mean, maxv
}

// baseline measures the machine with nothing of ours running.
func baseline(d time.Duration) Load {
	var l Load
	s := startSampler()
	time.Sleep(d)
	l.BaselineCPUPct, _ = s.finish()
	foreignLoad(&l)
	l.Quiet = l.BaselineCPUPct < 15 && l.ForeignBenchLock == ""
	return l
}

func cpuAndMemory() (string, float64) {
	if runtime.GOOS == "windows" {
		name := sh("powershell.exe", "-NoProfile", "-Command", "(Get-CimInstance Win32_Processor).Name")
		mem := sh("powershell.exe", "-NoProfile", "-Command", "(Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory")
		return strings.TrimSpace(name), parseGB(mem)
	}
	b, _ := os.ReadFile("/proc/cpuinfo")
	name := ""
	for _, line := range bytes.Split(b, []byte("\n")) {
		if k, v, ok := strings.Cut(string(line), ":"); ok && strings.TrimSpace(k) == "model name" {
			name = strings.TrimSpace(v)
			break
		}
	}
	return name, 0
}

func parseGB(s string) float64 {
	var n float64
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n = n*10 + float64(c-'0')
		}
	}
	return float64(int(n/(1<<30)*10)) / 10
}
