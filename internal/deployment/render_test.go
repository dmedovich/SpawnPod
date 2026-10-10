package deployment

import (
	"bytes"
	"github.com/dmedovich/spawnpod/internal/config"
	"io"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"os"
	"strings"
	"testing"
)

func TestInstallationIsCredentialSeparatedAndRestartSafe(t *testing.T) {
	b, err := os.ReadFile("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Kubernetes.Kubeconfig = "/private/admin-kubeconfig"
	image := "registry.example.com/runtime@sha256:" + strings.Repeat("a", 64)
	b, err = Render(cfg, image, image)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("admin-kubeconfig")) {
		t.Fatal("host credentials leaked into install")
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	var cm corev1.ConfigMap
	if err = decoder.Decode(&cm); err != nil {
		t.Fatal(err)
	}
	rendered, err := config.Decode(strings.NewReader(cm.Data["config.yaml"]))
	if err != nil {
		t.Fatal(err)
	}
	if rendered.Kubernetes.Kubeconfig != "" || rendered.Controller.MonitorAddress != ":9090" {
		t.Fatal("not in-cluster config")
	}
	var d appsv1.Deployment
	if err = decoder.Decode(&d); err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("unsafe rollout strategy")
	}
	spec := d.Spec.Template.Spec
	if *spec.AutomountServiceAccountToken || spec.ServiceAccountName != "spawnpod-controller" {
		t.Fatal("unexpected automatic token exposure")
	}
	for _, c := range spec.Containers {
		for _, m := range c.VolumeMounts {
			if c.Name == "runner" && m.Name == "api" {
				t.Fatal("runner receives Kubernetes credentials")
			}
			if c.Name == "controller" && m.Name == "runner" {
				t.Fatal("controller receives Runner secret")
			}
		}
		if !*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Fatal("unrestricted container")
		}
	}
	if d.Spec.Template.Annotations["checksum/config"] == "" {
		t.Fatal("config changes do not trigger rollout")
	}
	var svc corev1.Service
	if err = decoder.Decode(&svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatal("monitor publicly exposed")
	}
	if err = decoder.Decode(&svc); err != io.EOF {
		t.Fatalf("unexpected extra document: %v", err)
	}
	if _, err = Render(cfg, "runtime:latest", image); err == nil {
		t.Fatal("mutable runtime image accepted")
	}
}
