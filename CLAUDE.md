# RHAII Cluster Validation

## Project Overview

kubectl plugin for validating GPU cluster readiness for AI/ML workloads. Checks GPU hardware, RDMA networking, and cross-node bandwidth.

**Tier 1 (API checks)** will live in [odh-cli](https://github.com/opendatahub-io/odh-cli) (`kubectl odh validate`) — integration planned.
**Tier 2 (hardware checks)** live here — runs on GPU nodes via privileged per-node Jobs.

**Epic:** INFERENG-4707

## CLI

```bash
kubectl rhaii-validate gpu            # GPU hardware checks (driver, ECC)
kubectl rhaii-validate net-checks     # Per-node RDMA checks (devices, status, topology)
kubectl rhaii-validate net-ping       # RDMA connectivity mesh (ibv_rc_pingpong)
kubectl rhaii-validate net-bandwidth  # Network bandwidth tests (iperf3, tcp-lat, ib_write_bw)
kubectl rhaii-validate networking     # All networking: net-checks + net-ping + net-bandwidth
kubectl rhaii-validate deps           # Check operators/CRDs
kubectl rhaii-validate all            # Everything (deps + gpu + networking)
kubectl rhaii-validate clean          # Remove all validation resources
kubectl rhaii-validate --version

# Flags
--debug                               # Keep pods alive for debugging
-o json                                # JSON output
--image <img>                          # Override baked-in container image
--server-node <node>                   # Star topology (1 server, N clients)
--namespace <ns>                       # Custom namespace (default: rhaii-validation)
--nodes <n1,n2>                        # Specific nodes (default: all GPU nodes)
```

## Architecture

```
kubectl rhaii-validate all
    |
    +-- Auto-detects GPU vendor (NVIDIA/AMD) from node labels or allocatable
    +-- Auto-detects platform (AKS/EKS/CoreWeave/OCP)
    +-- Creates namespace + RBAC (+ OpenShift SCC if OCP)
    +-- Deploys per-node Jobs (host root mounted at /host)
    |       |
    |       +-- Per-node checks via chroot /host:
    |       |       +-- GPU driver (nvidia-smi / rocm-smi)
    |       |       +-- GPU ECC errors
    |       |       +-- GPU-NIC topology (NUMA affinity from sysfs)
    |       |       +-- RDMA devices (ibv_devices, fallback to sysfs)
    |       |       +-- RDMA NIC status (ibstat, fallback to sysfs)
    |       |
    +-- Collects JSON results from pod logs
    |
    +-- RDMA connectivity mesh (net-ping, pairwise topology):
    |       +-- ibv_rc_pingpong: per-NIC-pair connectivity (tools image)
    |       +-- Rail (same rail index) + cross-rail (different rail index)
    |       +-- RoCEv2: auto-discovers GID index from sysfs
    |       +-- InfiniBand: no GID needed
    |       +-- 3 retries per node pair, controller-managed
    |       +-- Ports: 18515 + N (one per NIC pair per node pair)
    |
    +-- Multi-node network test jobs (ring topology):
    |       +-- iperf3: TCP bandwidth per node pair (tools image)
    |       +-- tcp-lat: TCP latency per node pair (validator image, built-in)
    |       +-- ib_write_bw: RDMA per GPU-NIC pair (from topology, tools image)
    |       +-- RDMA skipped if no RDMA resource configured
    |       +-- Jobs use images: tools for iperf3/RDMA, validator for tcp-lat
    |
    +-- Stores JSON report in ConfigMap (persists after cleanup)
    +-- Prints table report with topology
    +-- Cleans up (Jobs + RBAC, ConfigMap + report preserved)
```

## Two Workload Types

| | Per-node Jobs (hardware checks) | Multi-node Jobs (network tests) |
|---|---|---|
| Purpose | Hardware checks | Network tests (bandwidth + latency) |
| Image | rhaii-validator | tools (iperf3/RDMA), validator (tcp-lat) |
| GPU request | None (privileged + chroot) | 1 per pod (auto-detected) |
| Host access | chroot /host | None (self-contained image) |
| Checks | `gpu` or `all` mode | `networking` or `all` mode |
| Tools | nvidia-smi, rocm-smi, ibv_devices | iperf3, ib_write_bw, ibv_rc_pingpong, tcp-lat |

## Project Structure

```
rhaii-cluster-validation/
├── cmd/agent/main.go              # CLI: gpu, networking, all, deps, clean, run (hidden)
├── pkg/
│   ├── checks/
│   │   ├── check.go               # Check interface, Result, NodeTopology, NodeReport
│   │   ├── gpu/
│   │   │   ├── driver.go          # NVIDIA driver check (chroot /host nvidia-smi)
│   │   │   ├── ecc.go             # NVIDIA ECC check
│   │   │   ├── amd_driver.go      # AMD driver check (chroot /host rocm-smi)
│   │   │   ├── amd_ecc.go         # AMD ECC/RAS check
│   │   │   └── topology.go        # GPU-NIC-NUMA topology discovery
│   │   ├── rdma/
│   │   │   ├── devices.go         # RDMA device discovery (ibv_devices/sysfs)
│   │   │   ├── status.go          # RDMA NIC status (ibstat/sysfs)
│   │   │   ├── rdmabw_job.go      # ib_write_bw job (-d device --use_cuda gpu)
│   │   │   ├── pingmesh_job.go    # ibv_rc_pingpong pairwise connectivity job
│   │   │   └── pingmesh_types.go  # Pingmesh report/result types
│   │   └── networking/
│   │       ├── iperf_job.go       # iperf3 TCP bandwidth job (tools image)
│   │       └── tcplat_job.go      # TCP latency job (uses built-in tcp-lat tool)
│   ├── config/
│   │   ├── platform.go            # PlatformConfig, GPUVendor, ResourceConfig
│   │   ├── detect.go              # Platform auto-detection (all nodes scanned)
│   │   ├── loader.go              # Load embedded + override config
│   │   └── platforms/*.yaml       # Per-platform configs
│   ├── controller/controller.go   # Orchestration: deploy, collect, topology, jobs, cleanup
│   ├── jobrunner/                 # Multi-node job framework (ring/star/pairwise, debug, scheduling)
│   └── runner/                    # Per-node check execution
├── manifests/image-references/
│   ├── jobs.yaml                  # Job container images (embedded via //go:embed)
│   └── embed.go
├── deploy/
│   ├── node-check-job.yaml        # Per-node Job template (host root at /host, hostPID)
│   └── rbac.yaml                  # RBAC (SCC added dynamically for OCP)
├── Dockerfile                     # UBI9 + util-linux (chroot)
└── Makefile
```

## Platform Config

Only configurable values. Everything else is auto-detected.

```yaml
platform: OCP

agent:
  requests:
    cpu: "500m"
    memory: "512Mi"
  annotations: {}

jobs:
  requests:
    cpu: "500m"
    memory: "512Mi"
    # RDMA resource — manually configured per cluster:
    #   rdma/ib: "8"
    #   nvidia.com/roce: "1"
  limits:
    # Device resources must be in both requests and limits:
    #   rdma/ib: "8"
    #   nvidia.com/roce: "1"
  annotations: {}

gpu:
  min_driver_version: "535.0"

thresholds:
  tcp_bandwidth_gbps:  # Higher is better: >= pass = PASS, >= warn = WARN, < warn = FAIL
    pass: 5
    warn: 1
  tcp_latency_ms:      # Lower is better: <= pass = PASS, <= warn = WARN, > warn = FAIL
    pass: 0.5
    warn: 1.0
  rdma_bandwidth_pd_gbps:
    pass: 180
    warn: 100

# Pingmesh (RDMA connectivity) config:
#   ping_iterations: 1          # ibv_rc_pingpong -n iterations
#   ping_timeout: 10            # per-test timeout in seconds
#   ping_gid_index: 3           # RoCE GID index (omit for auto-discovery)
```

## Auto-Detection

| What | How |
|------|-----|
| GPU vendor | Node labels (`nvidia.com/gpu.present`) or allocatable resources |
| GPU nodes | Label selector, fallback to allocatable scan (CoreWeave) |
| Platform | Node labels, provider ID (scans all nodes) |
| GPU count | `node.status.allocatable` |
| GPU-NIC topology | sysfs NUMA affinity |
| OpenShift SCC | Auto-created when OCP detected |

## GPU Vendor Support

| Vendor | Driver Check | ECC Check | Bandwidth Jobs |
|--------|-------------|-----------|----------------|
| NVIDIA | nvidia-smi (chroot) | nvidia-smi ECC query | iperf3, ib_write_bw |
| AMD | rocm-smi (chroot) | rocm-smi RAS query | Skipped (NVIDIA-only images) |

## Job Images

Defined in `manifests/image-references/jobs.yaml`, embedded at build time:

```yaml
images:
  default: "ghcr.io/llm-d/llm-d-rdma-tools-dev:latest"
  jobs:
    iperf3: ""   # uses default (includes iperf3)
    rdma: ""     # uses default
    nccl: ""     # uses default
```

**NOTE:** The `iperf3` image is used for iperf3 jobs. The TCP latency test uses the validator image with built-in `tcp-lat` tool (no external dependencies).

## Pingmesh (RDMA Connectivity)

`net-ping` tests RDMA data-plane connectivity between all GPU nodes using `ibv_rc_pingpong`.
It requires topology from a prior `net-checks` run (stored in the report ConfigMap).

**How it works:**
- Uses N-choose-2 pairwise scheduling (round-robin tournament) for all GPU node pairs
- Disjoint pairs run in parallel within each round
- Each pair tests every NIC-to-NIC combination (e.g., 8×8 = 64 tests per pair)
- 3 retry attempts per pair (controller-managed: redeploy server + client)
- Port range: `18515 + N` where N = NIC pair index (e.g., 18515–18578 for 8 NICs)

**Rail vs Cross-rail:**
- **Rail** (`rdma_conn_rail`): NIC pairs at the same rail index (e.g., GPU0↔NIC0 on both nodes). These share the same spine switch in a rail-optimized fabric.
- **Cross-rail** (`rdma_conn_xrail`): NIC pairs at different rail indices. Tests connectivity across fabric spines. Clusters with rail-only connectivity will PASS rail but FAIL/SKIP xrail.

**RoCEv2 vs InfiniBand:**
- RoCE: auto-discovers GID index from sysfs (prefers IPv4-mapped RoCE v2 GIDs); configurable via `ping_gid_index`
- IB: no GID needed, uses LID-based addressing natively

**Reports:**
- Summary in main report ConfigMap (`rhaii-validate-report`): `rdma_conn_rail` and `rdma_conn_xrail` status
- Detailed failures in separate ConfigMap (`rhaii-validate-pingmesh-failures`)
- Report merging: `net-ping` preserves topology/bandwidth data from previous runs

## Report Storage

JSON report stored in ConfigMap `rhaii-validate-report` after each run:

```bash
kubectl get cm rhaii-validate-report -n rhaii-validation -o jsonpath='{.data.report\.json}' | jq .
kubectl get cm rhaii-validate-report -n rhaii-validation -o jsonpath='{.data.report\.json}' | jq '.status'
```

## Build and Deploy

```bash
make build              # Build binary
make install            # Build + install as kubectl plugin
make container          # Build container image
make push               # Push container image
make deploy             # Build + install + run validation
make deploy-all         # Build + container + push + deploy
make clean              # Remove validation resources
make clean-all          # Remove everything including ConfigMap
make test               # Run unit tests
```

## Coding Conventions

- All per-node checks implement `Check` interface
- GPU/RDMA tools run on host via `chroot /host`
- GPU vendor auto-detected, not configured
- RDMA resource manually configured in `jobs.resources`
- Jobs implement optional interfaces: `Configurable`, `ThresholdConfigurable`, `ImageConfigurable`, `NameSuffixable`
- `apierrors.IsNotFound()` for K8s errors (not string matching)
- Deploy manifests embedded via `//go:embed`
- Binary name `rhaii-validator`, kubectl plugin name `kubectl-rhaii_validate`
- `run` subcommand is hidden (internal, used by per-node Jobs)

## Known TODOs

1. NCCL all-reduce job (framework ready, needs NCCL image)
2. Unit tests for jobrunner with fake clientset
3. AMD bandwidth jobs (need AMD-compatible images)
4. `deps` subcommand (check GPU Operator, Network Operator, device plugins)
5. Per-NIC RDMA testing on bare metal (test all 8 NICs independently)
