package loadgen

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Machine labels the host a measurement ran on. Latency numbers without it
// are not comparable, so every report carries one.
type Machine struct {
	CPU              string `json:"cpu"`
	PerformanceCores int    `json:"performance_cores,omitempty"`
	EfficiencyCores  int    `json:"efficiency_cores,omitempty"`
	LogicalCPUs      int    `json:"logical_cpus"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	GoVersion        string `json:"go_version"`
	GOMAXPROCS       int    `json:"gomaxprocs"`
}

// DetectMachine reads the CPU model and core layout from the OS. Fields it
// cannot determine are left empty rather than guessed.
func DetectMachine() Machine {
	m := Machine{
		CPU:         "unknown",
		LogicalCPUs: runtime.NumCPU(),
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		GoVersion:   runtime.Version(),
		GOMAXPROCS:  runtime.GOMAXPROCS(0),
	}
	switch runtime.GOOS {
	case "darwin":
		if s := sysctl("machdep.cpu.brand_string"); s != "" {
			m.CPU = s
		}
		// Apple silicon reports performance and efficiency clusters as
		// perflevel0 and perflevel1. Intel Macs have neither key.
		m.PerformanceCores, _ = strconv.Atoi(sysctl("hw.perflevel0.physicalcpu"))
		m.EfficiencyCores, _ = strconv.Atoi(sysctl("hw.perflevel1.physicalcpu"))
	case "linux":
		if s := linuxCPUModel(); s != "" {
			m.CPU = s
		}
	}
	return m
}

func (m Machine) String() string {
	cores := fmt.Sprintf("%d logical CPUs", m.LogicalCPUs)
	if m.PerformanceCores > 0 {
		cores = fmt.Sprintf("%dP+%dE cores, %s", m.PerformanceCores, m.EfficiencyCores, cores)
	}
	return fmt.Sprintf("%s (%s), %s/%s, %s, GOMAXPROCS=%d",
		m.CPU, cores, m.OS, m.Arch, m.GoVersion, m.GOMAXPROCS)
}

func sysctl(key string) string {
	out, err := exec.Command("sysctl", "-n", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func linuxCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(key) == "model name" {
			return strings.TrimSpace(val)
		}
	}
	return ""
}
