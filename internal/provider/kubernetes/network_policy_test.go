package kubernetes

import (
	"net/netip"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSourceWorkloadPolicyRequiresCombinedIdentity(t *testing.T) {
	policy, err := BuildSourceProxyWorkloadPolicy("proxy-infra", 3128)
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := BuildSourceProxyPolicy(netip.MustParseAddr("10.43.0.50"), 3128)
	expected.Spec.Egress[0].To = []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "proxy-infra"}},
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "spawnpod-source-proxy", "spawnpod.dev/component": "source-proxy"}},
	}}
	if !reflect.DeepEqual(policy.Spec, expected.Spec) {
		t.Fatalf("proxy identity or port broadened: %+v", policy.Spec)
	}
	for _, tc := range []struct {
		namespace string
		port      int32
	}{{"", 3128}, {"infra,ci", 3128}, {"infra", 0}, {"infra", 65536}} {
		if _, err := BuildSourceProxyWorkloadPolicy(tc.namespace, tc.port); err == nil {
			t.Fatalf("invalid workload endpoint accepted: %+v", tc)
		}
	}
}

func TestSourcePolicyPinsEndpointAndSelectsOnlySourceJobs(t *testing.T) {
	for _, address := range []string{"10.43.0.50", "2001:db8::50"} {
		ip := netip.MustParseAddr(address)
		policy, err := BuildSourceProxyPolicy(ip, 3128)
		if err != nil {
			t.Fatal(err)
		}
		spec := policy.Spec
		if policy.Name != SourceProxyPolicyName || len(spec.PodSelector.MatchLabels) != 2 || spec.PodSelector.MatchLabels[ownerLabel] != "true" || spec.PodSelector.MatchLabels[networkLabel] != "source" || len(spec.PodSelector.MatchExpressions) != 0 {
			t.Fatal("source policy selects unapproved workloads")
		}
		if len(spec.PolicyTypes) != 1 || spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress || len(spec.Ingress) != 0 || len(spec.Egress) != 1 {
			t.Fatal("unexpected source policy directions")
		}
		rule := spec.Egress[0]
		if len(rule.To) != 1 || rule.To[0].IPBlock == nil || rule.To[0].PodSelector != nil || rule.To[0].NamespaceSelector != nil || rule.To[0].IPBlock.CIDR != netip.PrefixFrom(ip, ip.BitLen()).String() || len(rule.To[0].IPBlock.Except) != 0 {
			t.Fatal("source policy broadens endpoint")
		}
		if len(rule.Ports) != 1 || rule.Ports[0].Port == nil || rule.Ports[0].Port.IntVal != 3128 || rule.Ports[0].EndPort != nil || rule.Ports[0].Protocol == nil || *rule.Ports[0].Protocol != corev1.ProtocolTCP {
			t.Fatal("source policy broadens port/protocol")
		}
	}
	for _, tc := range []struct {
		ip   netip.Addr
		port int32
	}{{netip.Addr{}, 3128}, {netip.MustParseAddr("127.0.0.1"), 3128}, {netip.MustParseAddr("224.0.0.1"), 3128}, {netip.MustParseAddr("10.43.0.50"), 0}, {netip.MustParseAddr("10.43.0.50"), 65536}} {
		if _, err := BuildSourceProxyPolicy(tc.ip, tc.port); err == nil {
			t.Fatalf("invalid proxy endpoint accepted: %+v", tc)
		}
	}
}

func TestServicePoliciesSelectOnlyMatchingJobAndService(t *testing.T) {
	spec := validServiceSpec()
	deny := BuildDefaultDenyPolicy()
	if deny.Name != DefaultDenyPolicyName || len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PodSelector.MatchExpressions) != 0 || len(deny.Spec.PolicyTypes) != 2 || len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 0 {
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
