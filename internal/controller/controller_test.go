package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/config"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

type countReconciler struct{ calls atomic.Int32 }

func exampleControllerConfig(t *testing.T) config.Config {
	t.Helper()
	data, err := os.ReadFile("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Decode(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func preflightClient(t *testing.T, cfg config.Config) *fake.Clientset {
	t.Helper()
	policy, binding, err := admission.Objects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	limits, err := admission.ContainerLimits(cfg)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := admission.NamespaceQuota(cfg)
	if err != nil {
		t.Fatal(err)
	}
	deny := provider.BuildDefaultDenyPolicy()
	deny.Namespace = cfg.Kubernetes.Namespace
	return fake.NewClientset(policy, binding, limits, quota, deny)
}

func (r *countReconciler) ReconcileOnce(context.Context) error { r.calls.Add(1); return nil }

func TestReconcileLoopStartsImmediatelyAndStops(t *testing.T) {
	r := &countReconciler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ReconcileLoop(ctx, r, 5*time.Millisecond, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	deadline := time.After(time.Second)
	for r.calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("reconciliation did not repeat")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconciliation did not stop")
	}
}

func TestPreflightChecksNamespacesAndAccess(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	if err := Preflight(context.Background(), cfg, client); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("denied") })
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("expected Pod list error")
	}
}

func TestPreflightSourcePolicyRejectsPeerBroadening(t *testing.T) {
	for _, mode := range []string{"ip", "pod-selector"} {
		t.Run(mode, func(t *testing.T) {
			cfg := exampleControllerConfig(t)
			profile := cfg.Profiles["default"]
			profile.Network = "source"
			cfg.Profiles["default"] = profile
			cfg.SourceProxy, cfg.SourceProxyPolicy = "http://10.43.0.50:3128", mode
			client := preflightClient(t, cfg)
			policy, err := cfg.SourceProxyNetworkPolicy()
			if err != nil {
				t.Fatal(err)
			}
			policy.Namespace = cfg.Kubernetes.Namespace
			if _, err := client.NetworkingV1().NetworkPolicies(policy.Namespace).Create(context.Background(), policy, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := Preflight(context.Background(), cfg, client); err != nil {
				t.Fatal(err)
			}
			policy.Spec.Egress[0].To = []networkingv1.NetworkPolicyPeer{{}}
			if _, err := client.NetworkingV1().NetworkPolicies(policy.Namespace).Update(context.Background(), policy, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := Preflight(context.Background(), cfg, client); err == nil || !strings.Contains(err.Error(), "source proxy NetworkPolicy differs") {
				t.Fatalf("widened peer accepted: %v", err)
			}
		})
	}
}

func TestPreflightAlwaysRequiresDefaultDeny(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	if err := client.NetworkingV1().NetworkPolicies("ci").Delete(context.Background(), provider.DefaultDenyPolicyName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("preflight accepted missing default-deny policy")
	}
	policy := provider.BuildDefaultDenyPolicy()
	if _, err := client.NetworkingV1().NetworkPolicies("ci").Create(context.Background(), policy, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Egress = append(policy.Spec.Egress, networkingv1.NetworkPolicyEgressRule{})
	if _, err := client.NetworkingV1().NetworkPolicies("ci").Update(context.Background(), policy, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("preflight accepted permissive policy")
	}
}

func TestDefaultDenyAcceptsOnlyCoveringSelectorsAndClosedRules(t *testing.T) {
	cases := []struct {
		name   string
		change func(*networkingv1.NetworkPolicy)
		valid  bool
	}{
		{"namespace", func(p *networkingv1.NetworkPolicy) {}, true},
		{"managed", func(p *networkingv1.NetworkPolicy) {
			p.Spec.PodSelector.MatchLabels = map[string]string{"spawnpod.dev/managed": "true"}
		}, true},
		{"wrong value", func(p *networkingv1.NetworkPolicy) {
			p.Spec.PodSelector.MatchLabels = map[string]string{"spawnpod.dev/managed": "false"}
		}, false},
		{"subset", func(p *networkingv1.NetworkPolicy) {
			p.Spec.PodSelector.MatchLabels = map[string]string{"spawnpod.dev/managed": "true", "app": "job"}
		}, false},
		{"expression", func(p *networkingv1.NetworkPolicy) {
			p.Spec.PodSelector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpExists}}
		}, false},
		{"ingress allow all", func(p *networkingv1.NetworkPolicy) { p.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{}} }, false},
		{"egress allow all", func(p *networkingv1.NetworkPolicy) { p.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}} }, false},
		{"one direction", func(p *networkingv1.NetworkPolicy) {
			p.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
		}, false},
		{"duplicate direction", func(p *networkingv1.NetworkPolicy) {
			p.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress, networkingv1.PolicyTypeEgress}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := provider.BuildDefaultDenyPolicy()
			tc.change(p)
			if validDefaultDeny(p) != tc.valid {
				t.Fatal("unexpected validation result")
			}
			if tc.valid {
				cfg := exampleControllerConfig(t)
				client := preflightClient(t, cfg)
				if _, err := client.NetworkingV1().NetworkPolicies(cfg.Kubernetes.Namespace).Update(context.Background(), p, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				if err := Preflight(context.Background(), cfg, client); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if validDefaultDeny(nil) {
		t.Fatal("accepted missing policy")
	}
}

func TestPreflightRejectsImageAdmissionDrift(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := fake.NewClientset()
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("missing admission policy was accepted")
	}
	client = preflightClient(t, cfg)
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.Background(), admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	policy.Spec.Validations[0].Expression = "true"
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(context.Background(), policy, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("admission allowlist drift was accepted")
	}
	client = preflightClient(t, cfg)
	profile := cfg.Profiles["default"]
	profile.Image = "registry.example.com/other@sha256:" + strings.Repeat("e", 64)
	cfg.Profiles["default"] = profile
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("changed profile image was accepted without admission update")
	}
}

func TestPreflightAcceptsAPIServerMatchDefaults(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.Background(), admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	scope := admissionv1.AllScopes
	match := admissionv1.Equivalent
	policy.Spec.MatchConstraints.ResourceRules[0].Scope = &scope
	policy.Spec.MatchConstraints.MatchPolicy = &match
	policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{}
	policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{}
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(context.Background(), policy, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(context.Background(), admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	binding.Spec.MatchResources.MatchPolicy = &match
	binding.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{}
	if _, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Update(context.Background(), binding, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err != nil {
		t.Fatalf("API server match defaults caused drift: %v", err)
	}
}

func TestPreflightRejectsContainerLimitDrift(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	limits, err := client.CoreV1().LimitRanges("ci").Get(context.Background(), admission.LimitRangeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	limits.Spec.Limits[0].Max[corev1.ResourceCPU] = resource.MustParse("100")
	if _, err := client.CoreV1().LimitRanges("ci").Update(context.Background(), limits, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("broadened container limit was accepted")
	}
	client = preflightClient(t, cfg)
	limits, err = client.CoreV1().LimitRanges("ci").Get(context.Background(), admission.LimitRangeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	delete(limits.Spec.Limits[0].Default, corev1.ResourceEphemeralStorage)
	if _, err := client.CoreV1().LimitRanges("ci").Update(context.Background(), limits, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("missing ephemeral-storage default was accepted")
	}
}

func TestPreflightRejectsNamespaceQuotaDrift(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	quota, err := client.CoreV1().ResourceQuotas("ci").Get(context.Background(), admission.QuotaName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	quota.Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("100")
	if _, err := client.CoreV1().ResourceQuotas("ci").Update(context.Background(), quota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := Preflight(context.Background(), cfg, client); err == nil {
		t.Fatal("broadened namespace budget was accepted")
	}
}

func TestRunServesOnlyWhileLeaseHeld(t *testing.T) {
	data, err := os.ReadFile("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(t.TempDir(), "private", "plugin.sock")
	cfg, err := config.Decode(strings.NewReader(strings.Replace(string(data), "/run/spawnpod/backend.sock", socket, 1)))
	if err != nil {
		t.Fatal(err)
	}
	// HTTP behavior is tested separately; do not claim a fixed host port here.
	cfg.Controller.MonitorAddress = ""
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	client := preflightClient(t, cfg)
	go func() {
		done <- Run(ctx, cfg, client, &rest.Config{Host: "https://example.invalid"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("controller stopped: %v", err)
		case <-deadline:
			cancel()
			t.Fatal("leader did not create socket")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not stop")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket still exists: %v", err)
	}
}

func TestStaleAgentDiagnosticSharedWithDoctor(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	cfg.AgentImage = cfg.AgentImage[:strings.LastIndex(cfg.AgentImage, ":")+1] + strings.Repeat("e", 64)
	err := Preflight(context.Background(), cfg, client)
	if err == nil || !strings.Contains(err.Error(), "admission.policy.validations[7].expression") {
		t.Fatalf("missing failed predicate: %v", err)
	}
	var output strings.Builder
	if err := Doctor(context.Background(), cfg, client, &output); err == nil || !strings.Contains(output.String(), "admission.policy.validations[7].expression") {
		t.Fatalf("doctor/startup diverge: %v %s", err, output.String())
	}
}
