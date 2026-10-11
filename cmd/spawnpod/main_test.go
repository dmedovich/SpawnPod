package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/config"
)

func TestConfigureClientImpersonatesRestrictedController(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	data := []byte("apiVersion: v1\nkind: Config\nclusters:\n- name: local\n  cluster:\n    server: https://127.0.0.1:6443\nusers:\n- name: admin\n  user:\n    token: test\ncontexts:\n- name: local\n  context:\n    cluster: local\n    user: admin\ncurrent-context: local\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Kubernetes: config.Kubernetes{Kubeconfig: path, LeaseNamespace: "infra"}}
	_, restConfig, err := configureClient(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if restConfig.Impersonate.UserName != "system:serviceaccount:infra:spawnpod-controller" || len(restConfig.Impersonate.Groups) != 3 {
		t.Fatalf("wrong impersonated identity: %+v", restConfig.Impersonate)
	}
	cfg.Kubernetes.Kubeconfig = ""
	if _, _, err := configureClient(cfg, true); err == nil {
		t.Fatal("accepted impersonation without explicit admin kubeconfig")
	}
}

func TestCommandFlagsDoNotLeakDoctorOptions(t *testing.T) {
	for _, command := range []string{"validate", "render-admission", "render-deployment", "smoke"} {
		if err := run([]string{command, "--strict"}); err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("%s exposes doctor-only flag: %v", command, err)
		}
	}
	if err := runUpgrade([]string{"apply"}); err == nil {
		t.Fatal("unimplemented apply accepted")
	}
}
func TestUpgradeRenderPreviewAndIdempotence(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := []string{"render", "--config", "../../deploy/config.example.yaml", "--image", cfg.AgentImage, "--runner-image", cfg.AgentImage, "--output-dir", dir}
	if err := runUpgrade(append(append([]string{}, args...), "--check")); err == nil {
		t.Fatal("preview did not report missing files")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatal("preview mutated output")
	}
	if err := runUpgrade(args); err != nil {
		t.Fatal(err)
	}
	if err := runUpgrade(append(append([]string{}, args...), "--check")); err != nil {
		t.Fatalf("render not idempotent: %v", err)
	}
}
