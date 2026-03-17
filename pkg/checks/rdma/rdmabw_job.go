package rdma

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/jobrunner"

	batchv1 "k8s.io/api/batch/v1"
)

// RDMABandwidthJob implements the Job interface for RDMA bandwidth testing via ib_write_bw.
type RDMABandwidthJob struct {
	Duration    int                  // test duration in seconds (default 10)
	Threshold   float64              // Gbps pass threshold
	PodCfg      *jobrunner.PodConfig // optional pod configuration
	ServerImage string               // optional custom server image (empty = use default)
	ClientImage string               // optional custom client image (empty = use default)
	Device      string               // RDMA device (e.g., "mlx5_0"), empty = auto
	UseCUDA     int                  // GPU ID for GPUDirect RDMA (-1 = disabled)
}

// NewRDMABandwidthJob creates an RDMA bandwidth job.
func NewRDMABandwidthJob(threshold float64, podCfg *jobrunner.PodConfig) *RDMABandwidthJob {
	return &RDMABandwidthJob{
		Duration: 10,
		Threshold: threshold,
		PodCfg:   podCfg,
		UseCUDA:  -1, // disabled by default
	}
}

func (j *RDMABandwidthJob) Name() string {
	if j.Device != "" {
		dev := strings.ReplaceAll(j.Device, "_", "-")
		if j.UseCUDA >= 0 {
			return fmt.Sprintf("ib-bw-gpu%d-%s", j.UseCUDA, dev)
		}
		return fmt.Sprintf("ib-bw-%s", dev)
	}
	return "ib-write-bw"
}

func (j *RDMABandwidthJob) SetPodConfig(cfg *jobrunner.PodConfig) {
	if cfg == nil {
		cfg = &jobrunner.PodConfig{
			ResourceRequests: make(map[string]string),
			ResourceLimits:   make(map[string]string),
		}
	}
	if cfg.ResourceLimits == nil {
		cfg.ResourceLimits = make(map[string]string)
	}
	// Device resources (GPU, RDMA) must have equal requests and limits (K8s requirement)
	// Auto-copy from requests to limits if missing
	for k, v := range cfg.ResourceRequests {
		if k == "cpu" || k == "memory" {
			continue
		}
		if _, ok := cfg.ResourceLimits[k]; !ok {
			cfg.ResourceLimits[k] = v
		}
	}
	// RDMA jobs need privileged access to InfiniBand/RoCE devices
	cfg.Privileged = true
	j.PodCfg = cfg
}
func (j *RDMABandwidthJob) SetThreshold(t float64)               { j.Threshold = t }

// Implement ImageConfigurable interface
func (j *RDMABandwidthJob) GetServerImage() string { return j.ServerImage }
func (j *RDMABandwidthJob) GetClientImage() string { return j.ClientImage }

// Setters for controller to apply config
func (j *RDMABandwidthJob) SetServerImage(img string) { j.ServerImage = img }
func (j *RDMABandwidthJob) SetClientImage(img string) { j.ClientImage = img }

func (j *RDMABandwidthJob) buildArgs() []string {
	args := []string{"ib_write_bw", "--duration", fmt.Sprintf("%d", j.Duration)}
	if j.Device != "" {
		args = append(args, "-d", j.Device)
	}
	if j.UseCUDA >= 0 {
		args = append(args, "--use_cuda", fmt.Sprintf("%d", j.UseCUDA))
	}
	return args
}

func (j *RDMABandwidthJob) ServerSpec(node, namespace, image string) (*batchv1.Job, error) {
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleServer, j.PodCfg,
		j.buildArgs())
}

func (j *RDMABandwidthJob) ClientSpec(node, namespace, image, serverIP string) (*batchv1.Job, error) {
	args := append(j.buildArgs(), serverIP)
	return jobrunner.BuildJobSpec(j.Name(), node, namespace, image, jobrunner.RoleClient, j.PodCfg,
		args)
}

func (j *RDMABandwidthJob) ParseResult(logs string) (*jobrunner.JobResult, error) {
	gbps, err := parseIBWriteBW(logs)
	if err != nil {
		return nil, err
	}

	r := &jobrunner.JobResult{
		Details: map[string]any{
			"bandwidth_gbps": fmt.Sprintf("%.1f", gbps),
		},
	}

	switch {
	case gbps >= j.Threshold:
		r.Status = checks.StatusPass
		r.Message = fmt.Sprintf("RDMA bandwidth: %.1f Gbps (threshold: %.0f Gbps)", gbps, j.Threshold)
	case gbps >= j.Threshold*0.4:
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("RDMA bandwidth: %.1f Gbps (below %.0f Gbps threshold)", gbps, j.Threshold)
	default:
		r.Status = checks.StatusFail
		r.Message = fmt.Sprintf("RDMA bandwidth: %.1f Gbps (well below %.0f Gbps threshold)", gbps, j.Threshold)
	}

	return r, nil
}

// parseIBWriteBW extracts the average bandwidth in Gbps from ib_write_bw output.
// The 4th column (index 3) is "BW average [MB/sec]", converted to Gbps.
func parseIBWriteBW(output string) (float64, error) {
	var lastBW float64
	found := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[3], 64); err == nil {
			lastBW = v
			found = true
		}
	}
	if !found {
		return 0, fmt.Errorf("no bandwidth value found in ib_write_bw output")
	}
	gbps := lastBW * 8 / 1000
	return gbps, nil
}
