package kubernetes

import (
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
)

func TestServicePoliciesSelectOnlyMatchingJobAndService(t *testing.T) {
	spec := validServiceSpec()
	deny := BuildDefaultDenyPolicy()
	if deny.Name != DefaultDenyPolicyName || deny.Spec.PodSelector.MatchLabels[ownerLabel] != "true" || len(deny.Spec.PolicyTypes) != 2 || len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
		t.Fatal("default deny does not cover managed Pods in both directions")
	}
	egress, ingress, err := BuildServicePolicies(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(egress.Name) > 63 || len(ingress.Name) > 63 || egress.Name == ingress.Name {
		t.Fatal("policy names are invalid or collide")
	}
	if len(egress.Spec.PolicyTypes) != 1 || egress.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(egress.Spec.Ingress) != 0 || len(egress.Spec.Egress) != 1 {
		t.Fatal("job policy grants unexpected direction")
	}
	if len(ingress.Spec.PolicyTypes) != 1 || ingress.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress || len(ingress.Spec.Egress) != 0 || len(ingress.Spec.Ingress) != 1 {
		t.Fatal("service policy grants unexpected direction")
	}
	jobSelector := egress.Spec.PodSelector
	if jobSelector.MatchLabels[environmentLabel] != spec.EnvironmentID || jobSelector.MatchLabels[ownerLabel] != "true" || len(jobSelector.MatchExpressions) != 1 || jobSelector.MatchExpressions[0].Key != serviceLabel {
		t.Fatal("job policy selects another environment or service Pod")
	}
	serviceSelector := ingress.Spec.PodSelector
	if serviceSelector.MatchLabels[environmentLabel] != spec.EnvironmentID || serviceSelector.MatchLabels[serviceLabel] != spec.Name {
		t.Fatal("service policy selects another service")
	}
	to := egress.Spec.Egress[0]
	from := ingress.Spec.Ingress[0]
	if len(to.To) != 1 || to.To[0].PodSelector == nil || to.To[0].PodSelector.MatchLabels[serviceLabel] != spec.Name || len(from.From) != 1 || from.From[0].PodSelector == nil || from.From[0].PodSelector.MatchLabels[environmentLabel] != spec.EnvironmentID {
		t.Fatal("policy peers are broader than approved job and service")
	}
	if len(to.Ports) != 1 || to.Ports[0].Port.IntVal != spec.Port || len(from.Ports) != 1 || from.Ports[0].Port.IntVal != spec.Port {
		t.Fatal("policies expose another port")
	}
}
