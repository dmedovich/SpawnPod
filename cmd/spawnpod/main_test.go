package main

import (
	"os"
	"path/filepath"
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
