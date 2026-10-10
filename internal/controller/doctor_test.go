package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/admission"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
