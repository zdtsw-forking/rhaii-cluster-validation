package checks

import (
	"context"
	"time"
)

// Check is the interface all validation checks must implement.
type Check interface {
	Name() string
	Category() string
	Run(ctx context.Context) Result
}

// Result represents the outcome of a single validation check.
type Result struct {
	Node        string `json:"node,omitempty"`
	Category    string `json:"category"`
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
	Details     any    `json:"details,omitempty"`
}

// Status represents the result of a check.
type Status string

const (
	StatusPass Status = "PASS"
	StatusWarn Status = "WARN"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// GPUNICPair represents a GPU paired with its closest RDMA NIC based on NUMA affinity.
type GPUNICPair struct {
	GPUID    int    `json:"gpu_id"`
	GPUName  string `json:"gpu_name,omitempty"`
	NUMAID   int    `json:"numa_id"`
	NICDev   string `json:"nic_dev"`   // e.g., "mlx5_0"
	NICNuma  int    `json:"nic_numa"`
	PCIeAddr string `json:"pcie_addr,omitempty"`
}

// NodeTopology holds the GPU-NIC-NUMA mapping for a node.
type NodeTopology struct {
	GPUCount int          `json:"gpu_count"`
	NICCount int          `json:"nic_count"`
	Pairs    []GPUNICPair `json:"pairs"`
}

// NodeReport is the complete output from an agent run on a single node.
type NodeReport struct {
	Node      string        `json:"node"`
	Timestamp time.Time     `json:"timestamp"`
	Results   []Result      `json:"results"`
	Topology  *NodeTopology `json:"topology,omitempty"`
}
