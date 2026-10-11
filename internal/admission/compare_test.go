package admission

import (
	"github.com/dmedovich/spawnpod/internal/config"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"testing"
)

func TestCompareSecurityFieldsAndDefaults(t *testing.T) {
	cfg, err := config.Load("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected, binding, err := Objects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, field string
		change      func(*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding)
	}{
		{"failure", "admission.policy.failurePolicy", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			v := admissionv1.Ignore
			p.Spec.FailurePolicy = &v
		}},
		{"namespace", "admission.binding.matchResources.namespaceSelector", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources.NamespaceSelector = &metav1.LabelSelector{}
		}},
		{"agent", "admission.policy.validations[7].expression", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.Validations[7].Expression = "sensitive-value"
		}},
		{"reason", "admission.policy.validations[0].reason", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			r := metav1.StatusReasonForbidden
			p.Spec.Validations[0].Reason = &r
		}},
		{"messageExpression", "admission.policy.validations[0].messageExpression", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.Validations[0].MessageExpression = "'sensitive-value'"
		}},
		{"variables", "admission.policy.variables", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.Variables = []admissionv1.Variable{{Name: "hidden", Expression: "true"}}
		}},
		{"scope", "admission.policy.matchConstraints.resourceRules[0].scope", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			s := admissionv1.NamespacedScope
			p.Spec.MatchConstraints.ResourceRules[0].Scope = &s
		}},
		{"actions", "admission.binding.validationActions", func(p *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.ValidationActions = append(b.Spec.ValidationActions, admissionv1.Warn)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, b := expected.DeepCopy(), binding.DeepCopy()
			tc.change(p, b)
			err := Compare(p, expected, b, binding)
			if err == nil || !strings.Contains(err.Error(), tc.field) || strings.Contains(err.Error(), "sensitive-value") {
				t.Fatalf("unsafe/unhelpful diagnostic: %v", err)
			}
		})
	}
	p, b := expected.DeepCopy(), binding.DeepCopy()
	normalizeMatch(p.Spec.MatchConstraints)
	normalizeMatch(b.Spec.MatchResources)
	if err := Compare(p, expected, b, binding); err != nil {
		t.Fatal(err)
	}
}
