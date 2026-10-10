package kubernetes

import (
	"errors"
	"net/netip"

	"github.com/dmedovich/spawnpod/internal/engine"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const DefaultDenyPolicyName = "spawnpod-default-deny"
const SourceProxyPolicyName = "spawnpod-source-proxy"

// BuildSourceProxyPolicy admits TCP only to the operator-pinned proxy IP and
// port for job Pods that selected the source profile.
func BuildSourceProxyPolicy(ip netip.Addr, port int32) (*networkingv1.NetworkPolicy, error) {
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.Zone() != "" || port < 1 || port > 65535 {
		return nil, errors.New("invalid source proxy endpoint")
	}
	bits := 32
	if ip.Is6() {
		bits = 128
	}
	protocol := corev1.ProtocolTCP
	value := intstr.FromInt32(port)
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: SourceProxyPolicyName},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{ownerLabel: "true", networkLabel: "source"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: netip.PrefixFrom(ip, bits).String()}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &value}}}},
		},
	}, nil
}

func servicePolicyNames(environmentID, serviceName string) (string, string) {
	return "e-" + environmentID + "-" + serviceName, "i-" + environmentID + "-" + serviceName
}

func checkServicePolicyIdentity(policy *networkingv1.NetworkPolicy, name, environmentID, serviceName string) error {
	if policy.Name != name || policy.Labels[ownerLabel] != "true" || policy.Labels[environmentLabel] != environmentID || policy.Labels[serviceLabel] != serviceName {
		return errors.New("service NetworkPolicy ownership mismatch")
	}
	return nil
}

// BuildDefaultDenyPolicy selects every spawnPod-managed Pod, including Pods
// created before individual service policies are installed.
func BuildDefaultDenyPolicy() *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultDenyPolicyName},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{ownerLabel: "true"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// BuildServicePolicies grants one job Pod access to one service Pod on the
// approved TCP port. The namespace default-deny policy remains in effect.
// The caller must install these policies before enabling Forgejo services.
func BuildServicePolicies(spec engine.ServiceSpec) (*networkingv1.NetworkPolicy, *networkingv1.NetworkPolicy, error) {
	if _, err := validateServiceSpec(spec); err != nil {
		return nil, nil, err
	}
	if len(spec.Name) > 27 {
		return nil, nil, errors.New("service name exceeds policy name limit")
	}
	jobSelector := metav1.LabelSelector{
		MatchLabels:      map[string]string{ownerLabel: "true", environmentLabel: spec.EnvironmentID},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: serviceLabel, Operator: metav1.LabelSelectorOpDoesNotExist}},
	}
	serviceSelector := metav1.LabelSelector{MatchLabels: serviceLabels(spec)}
	port := intstr.FromInt32(spec.Port)
	protocol := corev1.ProtocolTCP
	toService := networkingv1.NetworkPolicyPeer{PodSelector: &serviceSelector}
	fromJob := networkingv1.NetworkPolicyPeer{PodSelector: &jobSelector}
	labels := serviceLabels(spec)
	egressName, ingressName := servicePolicyNames(spec.EnvironmentID, spec.Name)
	jobEgress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: egressName, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: jobSelector,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{toService}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}}}},
		},
	}
	serviceIngress := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: ingressName, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: serviceSelector,
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{fromJob}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocol, Port: &port}}}},
		},
	}
	return jobEgress, serviceIngress, nil
}
