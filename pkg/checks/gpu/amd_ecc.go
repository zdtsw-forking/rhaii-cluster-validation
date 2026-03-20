package gpu

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// AMDECCCheck validates AMD GPU ECC/RAS error status using rocm-smi.
type AMDECCCheck struct {
	nodeName string
}

func NewAMDECCCheck(nodeName string) *AMDECCCheck {
	return &AMDECCCheck{nodeName: nodeName}
}

func (c *AMDECCCheck) Name() string     { return "gpu_ecc_status" }
func (c *AMDECCCheck) Category() string { return "gpu_hardware" }

func (c *AMDECCCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	output, err := exec.CommandContext(ctx, "rocm-smi", "--showrasinfo", "all").Output()
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("rocm-smi RAS query failed: %v", err)
		return r
	}

	errorsFound := parseROCmRASErrors(string(output))
	if len(errorsFound) > 0 {
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("RAS errors found: %s", strings.Join(errorsFound, "; "))
		r.Remediation = "Replace GPU or contact cloud provider"
		r.Details = map[string]any{"errors": errorsFound}
		return r
	}

	r.Status = checks.StatusPass
	r.Message = "No uncorrectable RAS errors detected"
	return r
}

// parseROCmRASErrors looks for non-zero uncorrectable error counts in rocm-smi RAS output.
func parseROCmRASErrors(output string) []string {
	var errors []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if strings.Contains(lower, "uncorrectable") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				if val != "0" && val != "N/A" && val != "" {
					errors = append(errors, strings.TrimSpace(line))
				}
			}
		}
	}
	return errors
}
