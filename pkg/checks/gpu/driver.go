package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// DriverCheck validates the NVIDIA GPU driver version.
type DriverCheck struct {
	nodeName   string
	minVersion string
}

func NewDriverCheck(nodeName, minVersion string) *DriverCheck {
	return &DriverCheck{
		nodeName:   nodeName,
		minVersion: minVersion,
	}
}

func (c *DriverCheck) Name() string     { return "gpu_driver_version" }
func (c *DriverCheck) Category() string { return "gpu_hardware" }

func (c *DriverCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := hostExec(ctx, "nvidia-smi",
		"--query-gpu=driver_version,name,memory.total",
		"--format=csv,noheader,nounits")
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("nvidia-smi failed: %v", err)
		r.Remediation = "Ensure NVIDIA GPU driver is installed and nvidia-smi is available"
		return r
	}

	info, err := parseDriverOutput(string(output))
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = err.Error()
		return r
	}

	r.Details = map[string]any{
		"driver_version":     info.DriverVersion,
		"gpu_product":        info.GPUName,
		"memory_total":       info.MemoryTotal,
		"gpu_count":          info.GPUCount,
		"min_driver_version": c.minVersion,
	}

	// Compare driver version against minimum
	if c.minVersion != "" && compareVersions(info.DriverVersion, c.minVersion) < 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("Driver version %s is below minimum %s", info.DriverVersion, c.minVersion)
		r.Remediation = fmt.Sprintf("Upgrade NVIDIA GPU driver to >= %s", c.minVersion)
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("NVIDIA driver: %s, GPU: %s (%s MiB), %d GPU(s)",
		info.DriverVersion, info.GPUName, info.MemoryTotal, info.GPUCount)

	return r
}

// driverInfo holds parsed nvidia-smi driver query output.
type driverInfo struct {
	DriverVersion string
	GPUName       string
	MemoryTotal   string
	GPUCount      int
}

// parseDriverOutput parses nvidia-smi CSV output for driver_version,name,memory.total.
func parseDriverOutput(output string) (*driverInfo, error) {
	reader := csv.NewReader(strings.NewReader(strings.TrimSpace(output)))
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 {
		return nil, fmt.Errorf("failed to parse nvidia-smi output")
	}

	fields := records[0]
	if len(fields) < 3 {
		return nil, fmt.Errorf("unexpected nvidia-smi output format")
	}

	return &driverInfo{
		DriverVersion: strings.TrimSpace(fields[0]),
		GPUName:       strings.TrimSpace(fields[1]),
		MemoryTotal:   strings.TrimSpace(fields[2]),
		GPUCount:      len(records),
	}, nil
}

// hostExec runs a command on the host filesystem.
// The DaemonSet mounts the host root at /host. We use chroot to run
// commands with the host's binaries and libraries (nvidia-smi, ibstat, etc.).
func hostExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	chrootArgs := []string{"/host", name}
	chrootArgs = append(chrootArgs, args...)
	return exec.CommandContext(ctx, "chroot", chrootArgs...).Output()
}

// compareVersions compares two dot-separated version strings numerically.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
func compareVersions(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		aNum := 0
		bNum := 0
		if i < len(aParts) {
			aNum, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bNum, _ = strconv.Atoi(bParts[i])
		}
		if aNum < bNum {
			return -1
		}
		if aNum > bNum {
			return 1
		}
	}
	return 0
}
