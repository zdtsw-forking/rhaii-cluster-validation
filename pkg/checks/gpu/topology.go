package gpu

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

// chrootHostExec runs a command on the host filesystem via chroot /host.
// Used for sysfs reads that need access to host device topology.
func chrootHostExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	chrootArgs := []string{"/host", name}
	chrootArgs = append(chrootArgs, args...)
	return exec.CommandContext(ctx, "chroot", chrootArgs...).Output()
}

// TopologyCheck discovers GPU-NIC-NUMA mapping on the node via nsenter.
type TopologyCheck struct {
	nodeName string
	topology *checks.NodeTopology // populated after Run()
}

func NewTopologyCheck(nodeName string) *TopologyCheck {
	return &TopologyCheck{nodeName: nodeName}
}

func (c *TopologyCheck) Name() string     { return "gpu_nic_topology" }
func (c *TopologyCheck) Category() string { return "topology" }

// Topology returns the discovered topology after Run().
func (c *TopologyCheck) Topology() *checks.NodeTopology { return c.topology }

func (c *TopologyCheck) Run(ctx context.Context) checks.Result {
	r := checks.Result{
		Node:     c.nodeName,
		Category: c.Category(),
		Name:     c.Name(),
	}

	// Discover GPUs and their NUMA nodes
	gpus, err := discoverGPUs(ctx)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("GPU discovery failed: %v", err)
		return r
	}

	// Discover RDMA NICs and their NUMA nodes
	nics, err := discoverNICs(ctx)
	if err != nil {
		r.Status = checks.StatusSkip
		r.Message = fmt.Sprintf("NIC discovery failed: %v", err)
		return r
	}

	// Build GPU-NIC pairs based on NUMA affinity
	pairs := buildPairs(gpus, nics)

	c.topology = &checks.NodeTopology{
		GPUCount: len(gpus),
		NICCount: len(nics),
		Pairs:    pairs,
	}

	r.Status = checks.StatusPass
	r.Details = c.topology

	var pairDescs []string
	for _, p := range pairs {
		pairDescs = append(pairDescs, fmt.Sprintf("GPU%d↔%s(NUMA%d)", p.GPUID, p.NICDev, p.NUMAID))
	}
	r.Message = fmt.Sprintf("%d GPU(s), %d NIC(s): %s", len(gpus), len(nics), strings.Join(pairDescs, ", "))

	return r
}

type gpuInfo struct {
	id   int
	name string
	numa int
}

type nicInfo struct {
	dev  string
	numa int
}

// discoverGPUs uses nvidia-smi to get GPU IDs and their NUMA nodes.
func discoverGPUs(ctx context.Context) ([]gpuInfo, error) {
	// Get GPU index and name
	output, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,gpu_name",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}

	var gpus []gpuInfo
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.SplitN(line, ",", 2)
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || id < 0 || id > 255 {
			continue
		}
		name := strings.TrimSpace(fields[1])

		// Get NUMA node from sysfs (ID validated as integer above, safe for path)
		numaOutput, err := chrootHostExec(ctx, "cat",
			fmt.Sprintf("/sys/class/nvidia/nvidia%d/numa_node", id))
		numa := -1
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		gpus = append(gpus, gpuInfo{id: id, name: name, numa: numa})
	}

	return gpus, nil
}

// discoverNICs finds RDMA devices and their NUMA nodes from sysfs.
func discoverNICs(ctx context.Context) ([]nicInfo, error) {
	output, err := chrootHostExec(ctx, "ls", "/sys/class/infiniband/")
	if err != nil {
		return nil, fmt.Errorf("no infiniband devices: %w", err)
	}

	var nics []nicInfo
	for _, dev := range strings.Fields(strings.TrimSpace(string(output))) {
		dev = strings.TrimSpace(dev)
		if dev == "" {
			continue
		}

		// Get NUMA node
		numaPath := filepath.Join("/sys/class/infiniband", dev, "device/numa_node")
		numaOutput, err := chrootHostExec(ctx, "cat", numaPath)
		numa := -1
		if err == nil {
			numa, _ = strconv.Atoi(strings.TrimSpace(string(numaOutput)))
		}

		nics = append(nics, nicInfo{dev: dev, numa: numa})
	}

	// Sort by device name for consistent ordering
	sort.Slice(nics, func(i, j int) bool { return nics[i].dev < nics[j].dev })

	return nics, nil
}

// buildPairs matches GPUs to NICs based on NUMA affinity.
// Each GPU is paired with a NIC on the same NUMA node.
func buildPairs(gpus []gpuInfo, nics []nicInfo) []checks.GPUNICPair {
	// Build a map of NUMA node → available NICs
	nicsByNuma := make(map[int][]nicInfo)
	for _, nic := range nics {
		nicsByNuma[nic.numa] = append(nicsByNuma[nic.numa], nic)
	}

	// Track which NICs have been assigned
	nicIdx := make(map[int]int) // numa -> next available index

	var pairs []checks.GPUNICPair
	for _, gpu := range gpus {
		pair := checks.GPUNICPair{
			GPUID:   gpu.id,
			GPUName: gpu.name,
			NUMAID:  gpu.numa,
		}

		// Find a NIC on the same NUMA node
		if available, ok := nicsByNuma[gpu.numa]; ok && nicIdx[gpu.numa] < len(available) {
			nic := available[nicIdx[gpu.numa]]
			pair.NICDev = nic.dev
			pair.NICNuma = nic.numa
			nicIdx[gpu.numa]++
		} else if len(nics) > 0 {
			// Fallback: use any available NIC (cross-NUMA)
			fallbackIdx := gpu.id % len(nics)
			pair.NICDev = nics[fallbackIdx].dev
			pair.NICNuma = nics[fallbackIdx].numa
		}

		pairs = append(pairs, pair)
	}

	return pairs
}
