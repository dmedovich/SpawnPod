package config

import (
	"bytes"
	"testing"
)

func TestInstallationNormalizesDefaultsWithoutChangingSource(t *testing.T) {
	cfg, err := Load("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Kubernetes.Kubeconfig = "/private/operator-config"
	cfg.Controller.TombstoneRetention = ""
	profile := cfg.Profiles["default"]
	profile.Network = ""
	cfg.Profiles["default"] = profile
	baseline, err := InstallationBytes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	alternative, err := InstallationConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	alternative.Controller.TombstoneRetention = "1440m"
	alternative.Controller.ReconcileInterval = "30000ms"
	cfg.Controller.ReconcileInterval = "30s"
	// All equivalent resource and default spellings describe the same installation.
	p := alternative.Profiles["default"]
	p.Network = ""
	p.MemoryRequest = "268435456"
	p.CPURequest = "0.25"
	alternative.Profiles["default"] = p
	original := cfg.Profiles["default"]
	if original.MemoryRequest != "256Mi" || original.CPURequest != "250m" {
		t.Fatal("update test with changed example")
	}
	result, err := InstallationBytes(alternative)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result, baseline) {
		t.Fatalf("equivalent effective configurations differ:\n%s\n%s", result, baseline)
	}
	if cfg.Kubernetes.Kubeconfig != "/private/operator-config" || cfg.Controller.TombstoneRetention != "" || cfg.Profiles["default"].Network != "" {
		t.Fatal("source config changed")
	}
}
