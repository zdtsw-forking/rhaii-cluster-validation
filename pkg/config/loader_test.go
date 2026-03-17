package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_DefaultsOnly(t *testing.T) {
	cfg, err := Load(PlatformAKS, "")
	if err != nil {
		t.Fatalf("Load(AKS, '') returned error: %v", err)
	}

	if cfg.Platform != PlatformAKS {
		t.Errorf("expected AKS, got %s", cfg.Platform)
	}
	if cfg.GPU.MinDriverVersion != "535.0" {
		t.Errorf("expected min driver 535.0, got %s", cfg.GPU.MinDriverVersion)
	}
}

func TestLoad_OverrideJobResources(t *testing.T) {
	dir := t.TempDir()
	overrideFile := filepath.Join(dir, "override.yaml")

	content := `
jobs:
  requests:
    cpu: "1"
    memory: "1Gi"
    nvidia.com/roce: "1"
  limits:
    nvidia.com/roce: "1"
`
	if err := os.WriteFile(overrideFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(PlatformAKS, overrideFile)
	if err != nil {
		t.Fatalf("Load with override returned error: %v", err)
	}

	if cfg.Jobs.Requests["nvidia.com/roce"] != "1" {
		t.Errorf("expected roce in requests, got %v", cfg.Jobs.Requests)
	}
	if cfg.Jobs.Limits["nvidia.com/roce"] != "1" {
		t.Errorf("expected roce in limits, got %v", cfg.Jobs.Limits)
	}
	if cfg.Jobs.Requests["cpu"] != "1" {
		t.Errorf("expected cpu override to 1, got %s", cfg.Jobs.Requests["cpu"])
	}
}

func TestLoad_WithOverrideFile(t *testing.T) {
	dir := t.TempDir()
	overrideFile := filepath.Join(dir, "override.yaml")

	content := `
gpu:
  min_driver_version: "550.0"
thresholds:
  tcp_bandwidth_gbps:
    pass: 50
`
	if err := os.WriteFile(overrideFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(PlatformAKS, overrideFile)
	if err != nil {
		t.Fatalf("Load with override returned error: %v", err)
	}

	// Overridden fields
	if cfg.GPU.MinDriverVersion != "550.0" {
		t.Errorf("expected min driver 550.0, got %s", cfg.GPU.MinDriverVersion)
	}
	if cfg.Thresholds.TCPBandwidth.Pass != 50 {
		t.Errorf("expected TCP pass 50, got %f", cfg.Thresholds.TCPBandwidth.Pass)
	}

	// Non-overridden fields should keep defaults
	if cfg.Thresholds.RDMABandwidthPD.Pass != 180 {
		t.Errorf("RDMA PD pass should remain 180, got %f", cfg.Thresholds.RDMABandwidthPD.Pass)
	}
}

func TestLoad_InvalidFile(t *testing.T) {
	_, err := Load(PlatformAKS, "/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	badFile := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badFile, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(PlatformAKS, badFile)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}
