// Package admission renders cluster admission rules from approved profiles.
// Generated policies must be installed by an administrator before serving.
package admission

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/config"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const PolicyName = "spawnpod-workload"
const LimitRangeName = "spawnpod-container-ceilings"
const QuotaName = "spawnpod-namespace-budget"

var shapeValidations = []admissionv1.Validation{
	{Expression: "has(object.metadata.labels) && 'spawnpod.dev/managed' in object.metadata.labels && object.metadata.labels['spawnpod.dev/managed'] == 'true' && 'spawnpod.dev/environment' in object.metadata.labels && object.metadata.labels['spawnpod.dev/environment'].matches('^[a-f0-9]{32}$')", Message: "spawnPod ownership labels are required"},
	{Expression: "object.spec.containers[0].name == 'service' ? (!('spawnpod.dev/network' in object.metadata.labels) || object.metadata.labels['spawnpod.dev/network'] == 'isolated') : ('spawnpod.dev/network' in object.metadata.labels && object.metadata.labels['spawnpod.dev/network'] in ['isolated', 'source'])", Message: "approved network profile label is required"},
	{Expression: "object.spec.serviceAccountName == 'spawnpod-job' && has(object.spec.automountServiceAccountToken) && object.spec.automountServiceAccountToken == false && (!has(object.spec.imagePullSecrets) || size(object.spec.imagePullSecrets) == 0)", Message: "only the tokenless job ServiceAccount is allowed"},
	{Expression: "has(object.spec.initContainers) && size(object.spec.initContainers) == 1 && size(object.spec.containers) == 1 && object.spec.initContainers[0].name == 'install-agent' && object.spec.containers[0].name in ['job', 'service']", Message: "only spawnPod job and service containers are allowed"},
	{Expression: "has(object.spec.volumes) && size(object.spec.volumes) <= 3 && object.spec.volumes.all(v, has(v.emptyDir)) && (!has(object.spec.hostAliases) || size(object.spec.hostAliases) <= 8)", Message: "only bounded emptyDir volumes are allowed"},
	{Expression: "object.spec.containers.all(c, !has(c.env) && !has(c.envFrom) && (!has(c.ports) || c.ports.all(p, !has(p.hostPort) || p.hostPort == 0))) && object.spec.initContainers.all(c, !has(c.env) && !has(c.envFrom))", Message: "PodSpec environment and host ports are forbidden"},
	{Expression: "has(object.spec.containers[0].command) && size(object.spec.containers[0].command) == 2 && object.spec.containers[0].command[0] == '/opt/spawnpod/agent' && ((object.spec.containers[0].name == 'job' && object.spec.containers[0].command[1] == 'serve') || (object.spec.containers[0].name == 'service' && object.spec.containers[0].command[1] == 'serve-service')) && has(object.spec.initContainers[0].command) && object.spec.initContainers[0].command == ['/spawnpod-agent', 'install', '/tools/agent']", Message: "only the spawnPod agent entry points are allowed"},
}

// ImageExpression uses only config-approved images; workflow image strings do
// not enter this expression. JSON string literals are valid CEL literals.
func ImageExpression(cfg config.Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	jobSet := map[string]bool{}
	serviceSet := map[string]bool{}
	for _, profile := range cfg.Profiles {
		jobSet[profile.Image] = true
		for _, service := range profile.Services {
			serviceSet[service.Image] = true
		}
	}
	if len(jobSet)+len(serviceSet) > 128 {
		return "", errors.New("too many images for admission policy")
	}
	jobs := imageList(jobSet)
	services := "false"
	if len(serviceSet) > 0 {
		services = "c.image in " + imageList(serviceSet)
	}
	agent, err := json.Marshal(cfg.AgentImage)
	if err != nil {
		return "", err
	}
	expression := fmt.Sprintf("object.spec.initContainers.all(c, c.image == %s) && object.spec.containers.all(c, (c.name == 'job' && c.image in %s) || (c.name == 'service' && %s))", agent, jobs, services)
	if len(expression) > 16*1024 {
		return "", errors.New("image admission expression is too large")
	}
	return expression, nil
}

func imageList(set map[string]bool) string {
	images := make([]string, 0, len(set))
	for image := range set {
		images = append(images, image)
	}
	sort.Strings(images)
	literals := make([]string, len(images))
	for i, image := range images {
		encoded, _ := json.Marshal(image)
		literals[i] = string(encoded)
	}
	return "[" + strings.Join(literals, ", ") + "]"
}

// Objects returns a fail-closed policy and binding scoped to the workload
// namespace label installed by deploy/security.yaml.
func Objects(cfg config.Config) (*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding, error) {
	expression, err := ImageExpression(cfg)
	if err != nil {
		return nil, nil, err
	}
	fail := admissionv1.Fail
	policy := &admissionv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metav1.ObjectMeta{Name: PolicyName},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionv1.MatchResources{ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
				Operations: []admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
				Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
			}}}},
			Validations: append(append([]admissionv1.Validation(nil), shapeValidations...), admissionv1.Validation{Expression: expression, Message: "Pod images differ from approved spawnPod profiles"}),
		},
	}
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: PolicyName},
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        PolicyName,
			ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
			MatchResources:    &admissionv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"spawnpod.dev/role": "workload"}}},
		},
	}
	return policy, binding, nil
}

// ContainerLimits creates a namespace LimitRange from the largest approved
// per-container limits. Profile-specific limits remain enforced by the adapter.
func ContainerLimits(cfg config.Config) (*corev1.LimitRange, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	maximum := corev1.ResourceList{}
	add := func(name corev1.ResourceName, value string) {
		parsed := resource.MustParse(value)
		current, ok := maximum[name]
		if !ok || parsed.Cmp(current) > 0 {
			maximum[name] = parsed
		}
	}
	for _, profile := range cfg.Profiles {
		add(corev1.ResourceCPU, profile.CPULimit)
		add(corev1.ResourceMemory, profile.MemoryLimit)
		add(corev1.ResourceEphemeralStorage, profile.StorageLimit)
		for _, service := range profile.Services {
			add(corev1.ResourceCPU, service.CPULimit)
			add(corev1.ResourceMemory, service.MemoryLimit)
			add(corev1.ResourceEphemeralStorage, service.StorageLimit)
		}
	}
	return &corev1.LimitRange{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"},
		ObjectMeta: metav1.ObjectMeta{Name: LimitRangeName, Namespace: cfg.Kubernetes.Namespace},
		// Kubernetes defaults omitted resources before quota admission. Using
		// the maxima as defaults makes missing resources consume the whole
		// per-container budget instead of bypassing ephemeral-storage quota.
		Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
			Type: corev1.LimitTypeContainer, Max: maximum,
			Default: maximum.DeepCopy(), DefaultRequest: maximum.DeepCopy(),
		}}},
	}, nil
}

// NamespaceQuota caps aggregate compute and API object counts for the
// dedicated workload namespace. The operator chooses the budget in config.
func NamespaceQuota(cfg config.Config) (*corev1.ResourceQuota, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &corev1.ResourceQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"},
		ObjectMeta: metav1.ObjectMeta{Name: QuotaName, Namespace: cfg.Kubernetes.Namespace},
		Spec:       corev1.ResourceQuotaSpec{Hard: cfg.Kubernetes.Quota.Hard()},
	}, nil
}

// Render writes the admission, per-container and aggregate budget objects.
func Render(cfg config.Config) ([]byte, error) {
	policy, binding, err := Objects(cfg)
	if err != nil {
		return nil, err
	}
	first, err := marshalManifest(policy)
	if err != nil {
		return nil, err
	}
	second, err := marshalManifest(binding)
	if err != nil {
		return nil, err
	}
	limits, err := ContainerLimits(cfg)
	if err != nil {
		return nil, err
	}
	third, err := marshalManifest(limits)
	if err != nil {
		return nil, err
	}
	quota, err := NamespaceQuota(cfg)
	if err != nil {
		return nil, err
	}
	fourth, err := marshalManifest(quota)
	if err != nil {
		return nil, err
	}
	manifest := append(append(first, []byte("---\n")...), second...)
	manifest = append(manifest, []byte("---\n")...)
	manifest = append(manifest, third...)
	manifest = append(manifest, []byte("---\n")...)
	manifest = append(manifest, fourth...)
	if cfg.SourceProxy != "" {
		ip, port, err := cfg.SourceProxyEndpoint()
		if err != nil {
			return nil, err
		}
		policy, err := provider.BuildSourceProxyPolicy(ip, port)
		if err != nil {
			return nil, err
		}
		encoded, err := marshalManifest(policy)
		if err != nil {
			return nil, err
		}
		manifest = append(manifest, []byte("---\n")...)
		manifest = append(manifest, encoded...)
	}
	return manifest, nil
}

func marshalManifest(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	delete(object, "status")
	return yaml.Marshal(object)
}
