# Build stage
FROM registry.access.redhat.com/ubi9/go-toolset:1.25 AS builder

WORKDIR /opt/app-root/src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -ldflags "-X main.version=${VERSION}" -o /opt/app-root/rhaii-validator ./cmd/agent/

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi:latest

LABEL name="rhaii-validator" \
      vendor="Red Hat" \
      summary="RHAII Cluster Validation Agent" \
      description="Per-node hardware validation agent for GPU, RDMA, and network checks"

# Install tools needed by the agent
RUN dnf install -y \
      util-linux \
      pciutils \
      && dnf clean all

COPY --from=builder /opt/app-root/rhaii-validator /usr/local/bin/rhaii-validator

# GPU/RDMA tools (nvidia-smi, ibstat, ibv_devices) run on the host via nsenter.
# No need to install them in the container - privileged pod + nsenter handles it.

USER 0

ENTRYPOINT ["rhaii-validator"]
