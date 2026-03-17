package gpu

import (
	"context"
	"fmt"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// AMDDriverCheck validates the AMD GPU driver version using rocm-smi.
type AMDDriverCheck struct {
	nodeName   string
	minVersion string
}

func NewAMDDriverCheck(nodeName, minVersion string) *AMDDriverCheck {
	return &AMDDriverCheck{
		nodeName:   nodeName,
		minVersion: minVersion,
	}
}

func (c *AMDDriverCheck) Name() string     { return "gpu_driver_version" }
func (c *AMDDriverCheck) Category() string { return "gpu_hardware" }

func (c *AMDDriverCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := hostExec(ctx, "rocm-smi", "--showdriverversion")
	if err != nil {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("rocm-smi failed: %v", err)
		r.Remediation = "Ensure AMD ROCm driver is installed and rocm-smi is available"
		return r
	}

	driverVersion := parseROCmDriverVersion(string(output))
	if driverVersion == "" {
		r.Status = checks.StatusFail
		r.Message = "Could not parse driver version from rocm-smi output"
		return r
	}

	// Get GPU info
	gpuOutput, _ := hostExec(ctx, "rocm-smi", "--showproductname")
	gpuName := parseROCmGPUName(string(gpuOutput))

	// Get GPU count
	countOutput, _ := hostExec(ctx, "rocm-smi", "--showid")
	gpuCount := countROCmGPUs(string(countOutput))

	r.Details = map[string]any{
		"driver_version":     driverVersion,
		"gpu_product":        gpuName,
		"gpu_count":          gpuCount,
		"min_driver_version": c.minVersion,
	}

	if c.minVersion != "" && compareVersions(driverVersion, c.minVersion) < 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("ROCm driver version %s is below minimum %s", driverVersion, c.minVersion)
		r.Remediation = fmt.Sprintf("Upgrade AMD ROCm driver to >= %s", c.minVersion)
		return r
	}

	r.Status = checks.StatusPass
	r.Message = fmt.Sprintf("AMD ROCm driver: %s, GPU: %s, %d GPU(s)", driverVersion, gpuName, gpuCount)
	return r
}

// parseROCmDriverVersion extracts driver version from "rocm-smi --showdriverversion" output.
func parseROCmDriverVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Driver version") || strings.Contains(line, "driver version") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

// parseROCmGPUName extracts GPU product name from "rocm-smi --showproductname" output.
func parseROCmGPUName(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Card series") || strings.Contains(line, "card series") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return "unknown"
}

// countROCmGPUs counts GPU entries from "rocm-smi --showid" output.
func countROCmGPUs(output string) int {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "GPU[") {
			count++
		}
	}
	if count == 0 {
		count = 1 // at least 1 if rocm-smi ran successfully
	}
	return count
}
