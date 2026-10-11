package deployment

import (
	"bytes"
	"errors"
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
	for _, volume := range spec.Volumes {
		if volume.Name == "runner" && (volume.Secret == nil || len(volume.Secret.Items) != 1 || volume.Secret.Items[0].Key != "runner.yaml" || volume.Secret.Items[0].Path != "runner.yaml") {
			t.Fatal("Runner Secret does not require the runner.yaml key")
		}
	}
	budget := InstallationBudget()
	for _, pair := range []struct {
		key      corev1.ResourceName
		requests bool
		name     corev1.ResourceName
	}{
		{corev1.ResourceRequestsCPU, true, corev1.ResourceCPU}, {corev1.ResourceLimitsCPU, false, corev1.ResourceCPU},
		{corev1.ResourceRequestsMemory, true, corev1.ResourceMemory}, {corev1.ResourceLimitsMemory, false, corev1.ResourceMemory},
	} {
		total := budget[pair.key].DeepCopy()
		total.Set(0)
		for _, container := range spec.Containers {
			resources := container.Resources.Limits
			if pair.requests {
				resources = container.Resources.Requests
			}
			total.Add(resources[pair.name])
		}
		if expected := budget[pair.key]; total.Cmp(expected) != 0 {
			t.Fatalf("doctor budget differs from rendered Deployment for %s", pair.key)
		}
	}
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
	if err = decoder.Decode(&svc); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra document: %v", err)
	}
	if _, err = Render(cfg, "runtime:latest", image); err == nil {
		t.Fatal("mutable runtime image accepted")
	}
}
