package installation

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/config"
)

func example(t *testing.T) *Bundle {
	t.Helper()
	c, err := config.Load("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(c, "v0.1.0-rc.6", c.AgentImage, c.AgentImage)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestBundleDeterminismAndRuntimeDependency(t *testing.T) {
	b := example(t)
	again, err := Build(b.Config, b.Index.Target, b.Config.AgentImage, b.Index.Images[1].Reference)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range b.Files {
		if !bytes.Equal(data, again.Files[name]) {
			t.Fatalf("non deterministic %s", name)
		}
	}
	changed := b.Config.AgentImage[:strings.LastIndex(b.Config.AgentImage, ":")+1] + strings.Repeat("e", 64)
	next, err := Build(b.Config, b.Index.Target, changed, b.Index.Images[1].Reference)
	if err != nil {
		t.Fatal(err)
	}
	if next.Config.AgentImage != changed || b.Config.AgentImage == changed {
		t.Fatal("source mutated or runtime not shared")
	}
	for _, name := range []string{"20-admission.yaml", "30-deployment.yaml", IndexName} {
		if bytes.Equal(b.Files[name], next.Files[name]) {
			t.Fatalf("missing image dependency in %s", name)
		}
	}
	if bytes.Contains(b.Files["30-deployment.yaml"], []byte("kubeconfig: /")) {
		t.Fatal("host path leaked")
	}
	for _, o := range b.Index.Inventory {
		if o.Kind == "Secret" {
			t.Fatal("bundle exports credentials")
		}
	}
}
func TestRenderOwnershipDriftAndRecovery(t *testing.T) {
	b := example(t)
	dir := t.TempDir()
	unrelated := filepath.Join(dir, "manual.yaml")
	if err := os.WriteFile(unrelated, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	changes, err := b.RenderFiles(dir, true)
	if err != nil || len(changes) != 5 {
		t.Fatalf("preview %v %v", changes, err)
	}
	if _, err := os.Stat(filepath.Join(dir, IndexName)); !os.IsNotExist(err) {
		t.Fatal("preview wrote inventory")
	}
	if _, err := b.RenderFiles(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := b.VerifyFiles(dir); err != nil {
		t.Fatal(err)
	}
	changes, err = b.RenderFiles(dir, false)
	if err != nil || len(changes) != 0 {
		t.Fatalf("not idempotent %v %v", changes, err)
	}
	next, err := Build(b.Config, "next", b.Config.AgentImage[:strings.LastIndex(b.Config.AgentImage, ":")+1]+strings.Repeat("e", 64), b.Index.Images[1].Reference)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an interruption after one manifest rename, before inventory commit.
	if err := os.WriteFile(filepath.Join(dir, "20-admission.yaml"), next.Files["20-admission.yaml"], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := next.RenderFiles(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := next.VerifyFiles(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RenderFiles(dir, false); err != nil {
		t.Fatalf("full previous bundle restore: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "20-admission.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "30-deployment.yaml"), []byte("manual edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := next.RenderFiles(dir, false); err == nil {
		t.Fatal("manual edit overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "20-admission.yaml"))
	if !bytes.Equal(before, after) {
		t.Fatal("partial write before conflict detection")
	}
	kept, _ := os.ReadFile(unrelated)
	if string(kept) != "keep" {
		t.Fatal("unrelated edit changed")
	}
}
func TestRenderRejectsSymlinkAndUnownedFile(t *testing.T) {
	b := example(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target")
	_ = os.WriteFile(outside, []byte("private"), 0600)
	if err := os.Symlink(outside, filepath.Join(dir, "00-security.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RenderFiles(dir, false); err == nil {
		t.Fatal("symlink accepted")
	}
	dir = t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "00-security.yaml"), []byte("private"), 0600)
	if _, err := b.RenderFiles(dir, false); err == nil {
		t.Fatal("unowned file overwritten")
	}
}

func TestInstallationRequiresSeparateNamespaces(t *testing.T) {
	b := example(t)
	source := b.Config
	source.Kubernetes.LeaseNamespace = source.Kubernetes.Namespace
	if _, err := Build(source, b.Index.Target, source.AgentImage, b.Index.Images[1].Reference); err == nil {
		t.Fatal("Controller would be subject to workload admission/default deny")
	}
}

func sourceBundle(t *testing.T, namespace string, mode ...string) *Bundle {
	t.Helper()
	cfg := example(t).Config
	cfg.Kubernetes.Namespace = namespace
	cfg.SourceProxy = "http://10.43.0.50:3128"
	if len(mode) != 0 {
		cfg.SourceProxyPolicy = mode[0]
	}
	p := cfg.Profiles["default"]
	p.Network = "source"
	cfg.Profiles["source"] = p
	b, err := Build(cfg, "v0.1.0-rc.7", cfg.AgentImage, cfg.AgentImage)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSourcePolicyBundleNamespaceAndOwnership(t *testing.T) {
	for _, tc := range []struct{ namespace, mode string }{{"ci", "ip"}, {"build-jobs", "ip"}, {"ci", "pod-selector"}, {"build-jobs", "pod-selector"}} {
		t.Run(tc.namespace+"/"+tc.mode, func(t *testing.T) {
			namespace := tc.namespace
			b := sourceBundle(t, namespace, tc.mode)
			count := 0
			for _, item := range b.Index.Inventory {
				if item.Kind == "NetworkPolicy" && item.Name == "spawnpod-source-proxy" {
					count++
					if item.Namespace != namespace || item.File != "20-admission.yaml" || item.Phase != "POLICY" {
						t.Fatalf("incorrect source policy inventory: %+v", item)
					}
				}
			}
			if count != 1 {
				t.Fatalf("expected one source policy, found %d", count)
			}
			objects, err := DecodeObjects(b.Files["20-admission.yaml"])
			if err != nil {
				t.Fatal(err)
			}
			for _, obj := range objects {
				if obj.GetKind() == "NetworkPolicy" && obj.GetNamespace() != namespace {
					t.Fatalf("policy YAML namespace: %q", obj.GetNamespace())
				}
			}
			again, err := Build(b.Config, b.Index.Target, b.Config.AgentImage, b.Config.AgentImage)
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range b.Files {
				if !bytes.Equal(data, again.Files[name]) {
					t.Fatalf("non deterministic source bundle: %s", name)
				}
			}
		})
	}
	for _, item := range example(t).Index.Inventory {
		if item.Kind == "NetworkPolicy" && item.Name != "spawnpod-default-deny" {
			t.Fatalf("isolated bundle permits egress: %+v", item)
		}
	}
}
