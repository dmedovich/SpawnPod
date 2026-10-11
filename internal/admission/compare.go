package admission

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Compare checks the entire security specification. Only documented API match
// defaults are normalized; validation reasons, variables and audit annotations
// remain part of the comparison. Diagnostics never include CEL/config values.
func Compare(policy, expected *admissionv1.ValidatingAdmissionPolicy, binding, expectedBinding *admissionv1.ValidatingAdmissionPolicyBinding) error {
	a, b := policy.DeepCopy(), expected.DeepCopy()
	normalizeMatch(a.Spec.MatchConstraints)
	normalizeMatch(b.Spec.MatchConstraints)
	if err := compareSpec("admission.policy", a.Spec, b.Spec); err != nil {
		return fmt.Errorf("installed workload admission policy differs from current profiles: %w", err)
	}
	x, y := binding.DeepCopy(), expectedBinding.DeepCopy()
	normalizeMatch(x.Spec.MatchResources)
	normalizeMatch(y.Spec.MatchResources)
	if err := compareSpec("admission.binding", x.Spec, y.Spec); err != nil {
		return fmt.Errorf("installed workload admission binding differs from required scope: %w", err)
	}
	return nil
}

func normalizeMatch(m *admissionv1.MatchResources) {
	if m == nil {
		return
	}
	if m.MatchPolicy == nil {
		v := admissionv1.Equivalent
		m.MatchPolicy = &v
	}
	if m.NamespaceSelector == nil {
		m.NamespaceSelector = &metav1.LabelSelector{}
	}
	if m.ObjectSelector == nil {
		m.ObjectSelector = &metav1.LabelSelector{}
	}
	for _, rules := range [][]admissionv1.NamedRuleWithOperations{m.ResourceRules, m.ExcludeResourceRules} {
		for i := range rules {
			if rules[i].Scope == nil {
				scope := admissionv1.AllScopes
				rules[i].Scope = &scope
			}
		}
	}
}

func compareSpec(path string, actual, expected any) error {
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(expected)
	var av, bv any
	_ = json.Unmarshal(a, &av)
	_ = json.Unmarshal(b, &bv)
	field := difference(path, av, bv)
	if field == "" {
		return nil
	}
	return fmt.Errorf("%s (expected spec sha256:%x; actual spec sha256:%x); regenerate admission from the mounted controller configuration and apply before switching Controller", field, sha256.Sum256(b), sha256.Sum256(a))
}

func difference(path string, a, b any) string {
	if reflect.DeepEqual(a, b) {
		return ""
	}
	if am, ok := a.(map[string]any); ok {
		if bm, ok := b.(map[string]any); ok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			ordered := make([]string, 0, len(keys))
			for k := range keys {
				ordered = append(ordered, k)
			}
			sort.Strings(ordered)
			for _, k := range ordered {
				if d := difference(path+"."+k, am[k], bm[k]); d != "" {
					return d
				}
			}
		}
	}
	if aa, ok := a.([]any); ok {
		if ba, ok := b.([]any); ok && len(aa) == len(ba) {
			for i := range aa {
				if d := difference(fmt.Sprintf("%s[%d]", path, i), aa[i], ba[i]); d != "" {
					return d
				}
			}
		}
	}
	return path
}
