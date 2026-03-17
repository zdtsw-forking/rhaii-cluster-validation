# Examples Directory

This directory contains **reference documentation and examples** for educational purposes.

## Important: Not Production Configuration

**DO NOT** use files in this directory for production configuration.

## Production Configuration Location

All production configurations are in:

```
pkg/config/platforms/
├── aks.yaml          ← Edit for Azure Kubernetes Service
├── eks.yaml          ← Edit for AWS Elastic Kubernetes
├── ocp.yaml          ← Edit for OpenShift Container Platform
└── coreweave.yaml    ← Edit for CoreWeave
```

These files are:
- Embedded in the binary at build time via `//go:embed`
- Version-controlled
- Platform-specific
- Not modifiable by end users at runtime

## Files in This Directory

| File | Purpose |
|------|---------|
| `configmap.yaml` | Example ConfigMap for runtime overrides |

See `../manifests/image-references/` for job image configuration reference.

## How to Customize Production Images

### Step 1: Edit Platform Config

```bash
# Edit the platform-specific YAML file
vim pkg/config/platforms/ocp.yaml
```

### Step 2: Update Images Section

```yaml
images:
  default: "your-registry.com/your-image:tag"
  jobs:
    iperf3: ""  # uses default
    rdma: "vendor/rdma-tools:v1"  # specific image
    nccl: "nvidia/nccl:latest"
```

### Step 3: Rebuild Binary

```bash
# Images are embedded during build
make build
```

### Step 4: Build Container

```bash
make container IMG=quay.io/yourorg/agent:v1
```

### Step 5: Deploy

```bash
make deploy IMG=quay.io/yourorg/agent:v1
```

The jobs will automatically use images from the embedded platform config.

## Why Not Use Examples Directory?

| Issue | Consequence |
|-------|-------------|
| Not embedded | Changes won't affect binary |
| Not version-controlled properly | Drift from source of truth |
| Confusing | Developers might edit wrong files |
| Not maintainable | Multiple sources of truth |

## Correct Architecture

```
Source of Truth (embedded):
  pkg/config/platforms/*.yaml
       ↓
  Built into binary via //go:embed
       ↓
  Loaded by config.Load()
       ↓
  Applied by controller
       ↓
  Used by jobs

Examples (documentation):
  examples/*.yaml
       ↓
  Reference only
  NOT loaded by code
  NOT embedded in binary
```

## Summary

- **Production configs:** `pkg/config/platforms/*.yaml`
- **Examples:** `examples/` (reference/documentation only)
- **Change images:** Edit platform YAML, rebuild binary
- **Source of truth:** Platform YAML files (embedded)
