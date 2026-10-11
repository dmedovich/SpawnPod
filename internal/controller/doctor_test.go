package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/deployment"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestDoctorChecksControllerIdentityAndObservedPolicies(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	falseValue := false
	_, err := client.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: cfg.Kubernetes.Namespace,
		Labels: map[string]string{
			"spawnpod.dev/role":                  "workload",
			"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.34",
			"pod-security.kubernetes.io/audit": "restricted", "pod-security.kubernetes.io/audit-version": "v1.34",
			"pod-security.kubernetes.io/warn": "restricted", "pod-security.kubernetes.io/warn-version": "v1.34",
		},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CoreV1().ServiceAccounts(cfg.Kubernetes.Namespace).Create(context.Background(), &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-job"}, AutomountServiceAccountToken: &falseValue,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(context.Background(), admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	policy.Generation = 1
	policy.Status.ObservedGeneration = 1
	policy.Status.TypeChecking = &admissionv1.TypeChecking{}
	if _, err = client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(context.Background(), policy, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	quota, err := client.CoreV1().ResourceQuotas(cfg.Kubernetes.Namespace).Get(context.Background(), admission.QuotaName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	quota.Status.Hard = quota.Spec.Hard.DeepCopy()
	if _, err = client.CoreV1().ResourceQuotas(cfg.Kubernetes.Namespace).Update(context.Background(), quota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).DeepCopy()
		review.Status.Allowed = true
		return true, review, nil
	})
	var out bytes.Buffer
	if err := Doctor(context.Background(), cfg, client, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "OK   RBAC create ci/pods/exec") || !strings.Contains(out.String(), "Doctor is read-only") {
		t.Fatalf("missing diagnostic output: %s", out.String())
	}
	// A complete set of passing checks is not success if the report cannot be written.
	out.Reset()
	if err := DoctorWithOptions(context.Background(), cfg, client, &out, DoctorOptions{Strict: true}); err == nil || !strings.Contains(out.String(), "WARN") {
		t.Fatalf("strict ignored unobserved quota usage: %v", err)
	}
	quota.Status.Used = corev1.ResourceList{}
	for name := range quota.Spec.Hard {
		quota.Status.Used[name] = resource.MustParse("0")
	}
	if _, err := client.CoreV1().ResourceQuotas(cfg.Kubernetes.Namespace).Update(context.Background(), quota, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := DoctorWithOptions(context.Background(), cfg, client, &out, DoctorOptions{Strict: true}); err != nil {
		t.Fatalf("strict failed without warnings: %v\n%s", err, out.String())
	}
	reader, writer := io.Pipe()
	_ = reader.CloseWithError(io.ErrClosedPipe)
	defer func() { _ = writer.Close() }()
	if err := Doctor(context.Background(), cfg, client, writer); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("missing output failure: %v", err)
	}
	client.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).DeepCopy()
		review.Status.Allowed = review.Spec.ResourceAttributes.Resource != "pods/exec"
		return true, review, nil
	})
	out.Reset()
	if err := Doctor(context.Background(), cfg, client, &out); err == nil || !strings.Contains(out.String(), "FAIL RBAC create ci/pods/exec") {
		t.Fatalf("missing denied exec diagnostic: %v\n%s", err, out.String())
	}
}

func TestQuotaPreflightUsesLiveUsage(t *testing.T) {
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "infra-budget"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("4")}}, Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("2100m")}}}
	warnings := quotaWarnings(quota, deployment.InstallationBudget())
	if len(warnings) != 1 || !strings.Contains(warnings[0], "available=1900m required=2") {
		t.Fatalf("missing quota exhaustion: %v", warnings)
	}
	quota.Spec.Hard[corev1.ResourceLimitsCPU] = resource.MustParse("4500m")
	if warnings := quotaWarnings(quota, deployment.InstallationBudget()); len(warnings) != 0 {
		t.Fatalf("sufficient quota rejected: %v", warnings)
	}
	delete(quota.Status.Used, corev1.ResourceLimitsCPU)
	if warnings := quotaWarnings(quota, deployment.InstallationBudget()); len(warnings) != 1 || !strings.Contains(warnings[0], "not observed") {
		t.Fatalf("missing usage accepted: %v", warnings)
	}
}

func TestDoctorInstallationContractDoesNotPrintSecrets(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-runner", Namespace: "infra"}, Data: map[string][]byte{"token": []byte("PRIVATE_TOKEN_MARKER")}}
	if _, err := client.CoreV1().Secrets("infra").Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := DoctorWithOptions(context.Background(), cfg, client, &out, DoctorOptions{Installation: true}); err == nil || !strings.Contains(out.String(), "requires a nonempty runner.yaml") {
		t.Fatalf("missing Secret key accepted: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "PRIVATE_TOKEN_MARKER") {
		t.Fatal("Secret leaked into diagnostic output")
	}
	secret.Data["runner.yaml"] = []byte("PRIVATE_TOKEN_MARKER")
	if _, err := client.CoreV1().Secrets("infra").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := doctorRunnerSecret(context.Background(), cfg, client); err != nil {
		t.Fatal(err)
	}
	manifest, err := deployment.Render(cfg, cfg.AgentImage, cfg.AgentImage)
	if err != nil {
		t.Fatal(err)
	}
	// Render's first document is the mounted configuration.
	var cm corev1.ConfigMap
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	if err := decoder.Decode(&cm); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps("infra").Create(context.Background(), &cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := doctorMountedConfig(context.Background(), cfg, client); err != nil {
		t.Fatal(err)
	}
	cm.Data["config.yaml"] = "token: PRIVATE_TOKEN_MARKER"
	if _, err := client.CoreV1().ConfigMaps("infra").Update(context.Background(), &cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := doctorMountedConfig(context.Background(), cfg, client); err == nil || strings.Contains(err.Error(), "PRIVATE_TOKEN_MARKER") {
		t.Fatalf("unsafe config diagnostic: %v", err)
	}
}

func TestDoctorReportsUnavailableAPI(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("*", "*", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API unavailable") })
	var out bytes.Buffer
	if err := Doctor(context.Background(), exampleControllerConfig(t), client, &out); err == nil || !strings.Contains(out.String(), "FAIL controller preflight") {
		t.Fatalf("missing API failure: %v", err)
	}
}

func TestDoctorRejectsUnobservedAdmissionAndQuota(t *testing.T) {
	cfg := exampleControllerConfig(t)
	client := preflightClient(t, cfg)
	if err := doctorTypeChecking(context.Background(), client); err == nil {
		t.Fatal("accepted policy without observed type checking")
	}
	if err := doctorQuotaStatus(context.Background(), cfg, client); err == nil {
		t.Fatal("accepted quota without observed hard limits")
	}
}
