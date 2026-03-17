package config

import (
	"testing"
)

func TestGetConfig_AllPlatforms(t *testing.T) {
	platforms := []Platform{PlatformAKS, PlatformEKS, PlatformCoreWeave, PlatformOCP}

	for _, p := range platforms {
		t.Run(string(p), func(t *testing.T) {
			cfg, err := GetConfig(p)
			if err != nil {
				t.Fatalf("GetConfig(%s) returned error: %v", p, err)
			}

			if cfg.Platform != p {
				t.Errorf("expected platform %s, got %s", p, cfg.Platform)
			}

			// Must have min driver version
			if cfg.GPU.MinDriverVersion == "" {
				t.Error("GPU.MinDriverVersion must not be empty")
			}

			// Agent must have resource requests
			if len(cfg.Agent.Requests) == 0 {
				t.Error("Agent.Requests must not be empty")
			}
			if cfg.Agent.Requests["cpu"] == "" {
				t.Error("Agent.Requests must have cpu")
			}
			if cfg.Agent.Requests["memory"] == "" {
				t.Error("Agent.Requests must have memory")
			}

			// Jobs must have resource requests
			if len(cfg.Jobs.Requests) == 0 {
				t.Error("Jobs.Requests must not be empty")
			}
			if cfg.Jobs.Requests["cpu"] == "" {
				t.Error("Jobs.Requests must have cpu")
			}
			if cfg.Jobs.Requests["memory"] == "" {
				t.Error("Jobs.Requests must have memory")
			}

			// Thresholds must be positive
			if cfg.Thresholds.TCPBandwidth.Pass <= 0 {
				t.Error("TCPBandwidth.Pass must be positive")
			}
			if cfg.Thresholds.RDMABandwidthPD.Pass <= 0 {
				t.Error("RDMABandwidthPD.Pass must be positive")
			}

			// Pass > Warn > Fail for bandwidth
			if cfg.Thresholds.TCPBandwidth.Pass <= cfg.Thresholds.TCPBandwidth.Warn {
				t.Error("TCPBandwidth: Pass must be > Warn")
			}
			if cfg.Thresholds.TCPBandwidth.Warn <= cfg.Thresholds.TCPBandwidth.Fail {
				t.Error("TCPBandwidth: Warn must be > Fail")
			}
		})
	}
}

func TestGetConfig_UnknownPlatform(t *testing.T) {
	cfg, err := GetConfig(PlatformUnknown)
	if err != nil {
		t.Fatalf("GetConfig(Unknown) returned error: %v", err)
	}

	if cfg.GPU.MinDriverVersion == "" {
		t.Error("Unknown platform should fall back to defaults with min_driver_version")
	}
}

func TestResourceConfig_RequestsAndLimits(t *testing.T) {
	cfg, err := GetConfig(PlatformOCP)
	if err != nil {
		t.Fatalf("GetConfig(OCP) returned error: %v", err)
	}

	// Agent: requests set, no limits by default
	if cfg.Agent.Requests["cpu"] != "500m" {
		t.Errorf("Agent cpu request = %q, want 500m", cfg.Agent.Requests["cpu"])
	}
	if cfg.Agent.Requests["memory"] != "512Mi" {
		t.Errorf("Agent memory request = %q, want 512Mi", cfg.Agent.Requests["memory"])
	}

	// Jobs: requests set, limits empty by default
	if cfg.Jobs.Requests["cpu"] != "500m" {
		t.Errorf("Jobs cpu request = %q, want 500m", cfg.Jobs.Requests["cpu"])
	}

	// No GPU/RDMA in default config — those are auto-detected or manual
	if _, ok := cfg.Jobs.Requests["nvidia.com/gpu"]; ok {
		t.Error("Jobs should not have nvidia.com/gpu in default config")
	}
	if _, ok := cfg.Jobs.Requests["nvidia.com/roce"]; ok {
		t.Error("Jobs should not have nvidia.com/roce in default config")
	}
}

func TestResourceConfig_Annotations(t *testing.T) {
	cfg, err := GetConfig(PlatformAKS)
	if err != nil {
		t.Fatalf("GetConfig(AKS) returned error: %v", err)
	}

	// Annotations should be initialized (empty, not nil)
	if cfg.Agent.Annotations == nil {
		t.Error("Agent.Annotations should not be nil")
	}
	if cfg.Jobs.Annotations == nil {
		t.Error("Jobs.Annotations should not be nil")
	}
}

func TestResourceConfig_WithRDMA(t *testing.T) {
	// Simulate a config with RDMA resources
	rc := ResourceConfig{
		Requests: map[string]string{
			"cpu":              "500m",
			"memory":           "512Mi",
			"nvidia.com/roce":  "1",
		},
		Limits: map[string]string{
			"nvidia.com/roce": "1",
		},
	}

	// RDMA in requests
	if rc.Requests["nvidia.com/roce"] != "1" {
		t.Error("RDMA should be in requests")
	}

	// RDMA in limits (must match requests for device resources)
	if rc.Limits["nvidia.com/roce"] != "1" {
		t.Error("RDMA should be in limits")
	}

	// CPU only in requests, not limits
	if _, ok := rc.Limits["cpu"]; ok {
		t.Error("CPU should not be in limits")
	}
}
