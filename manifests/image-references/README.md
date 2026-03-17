# Image References

This directory contains **production image configuration** for multi-node test jobs.

## Purpose

**PRODUCTION CONFIGURATION** - Defines container images used by all test jobs across all platforms.

## Production Image Configuration

Image references are defined in:

```
manifests/image-references/
└── jobs.yaml         <- Container images (PRODUCTION - embedded in binary)
```

Platform-specific settings are in:
```
pkg/config/platforms/
├── ocp.yaml          <- OpenShift platform config
├── aks.yaml          <- Azure platform config
├── eks.yaml          <- AWS platform config
└── coreweave.yaml    <- CoreWeave platform config
```

## Files

| File | Purpose |
|------|---------|
| `jobs.yaml` | Reference showing job image configuration structure |

## How Images Work

### Build Time (Embedded)

```
1. Edit manifests/image-references/jobs.yaml
2. Build binary: make build
3. Images embedded via //go:embed (shared across all platforms)
4. Binary contains all image refs
```

### Runtime (Automatic)

```
1. Controller detects platform (OCP/AKS/EKS/CoreWeave)
2. Loads embedded platform config (GPU/RDMA/thresholds)
3. Loads embedded image config (from manifests/image-references/jobs.yaml)
4. Reads images.default and images.jobs.*
5. Automatically applies to registered jobs
6. Jobs use configured images
```

## Example: Adding New Image

**Step 1:** Edit image YAML

```yaml
# manifests/image-references/jobs.yaml
images:
  default: "ghcr.io/llm-d/llm-d-rdma-tools-dev:latest"
  jobs:
    iperf3: ""
    rdma: "mellanox/perftest:latest"    # NEW
    nccl: "nvidia/nccl:2.17.8"          # NEW
```

**Step 2:** Rebuild

```bash
make build
make container IMG=quay.io/yourorg/agent:v1
```

**Step 3:** Deploy

```bash
make deploy IMG=quay.io/yourorg/agent:v1
```

Jobs automatically use the new images.

## Why Images Are Separate from Platform Configs

| Reason | Benefit |
|--------|---------|
| Not platform-specific | Same images used on OCP/AKS/EKS/CoreWeave |
| Single source of truth | DRY principle - edit once, applies everywhere |
| Clean separation | Platform configs for platform settings, images for images |
| Version-controlled | Changes tracked in git |

## Source of Truth

**Images:** `manifests/image-references/jobs.yaml` (this file - embedded in binary)
**Platform settings:** `pkg/config/platforms/*.yaml` (GPU paths, RDMA config, thresholds)

## Summary

- **Production images:** `manifests/image-references/jobs.yaml` (embedded - edit here)
- **Platform configs:** `pkg/config/platforms/*.yaml` (embedded - platform-specific settings)
- **Changes:** Edit jobs.yaml for images, rebuild binary for changes to take effect
