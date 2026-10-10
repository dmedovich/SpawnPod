package admission

import (
	"os"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/config"
	admissionv1 "k8s.io/api/admissionregistration/v1"
)

func admissionConfig(t *testing.T) config.Config {
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

func TestImagePolicyMatchesConfiguredImagesAndFailsClosed(t *testing.T) {
	cfg := admissionConfig(t)
	policy, binding, err := Objects(cfg)
	if err != nil {
		t.Fatal(err)
	}
	expression := policy.Spec.Validations[len(policy.Spec.Validations)-1].Expression
	if !strings.Contains(expression, cfg.AgentImage) || !strings.Contains(expression, cfg.Profiles["default"].Image) || !strings.Contains(expression, "c.name == 'service' && false") {
		t.Fatalf("configured images missing from policy: %s", expression)
	}
	if policy.Name != PolicyName || policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionv1.Fail || len(policy.Spec.Validations) != len(shapeValidations)+1 || binding.Spec.PolicyName != PolicyName || len(binding.Spec.ValidationActions) != 1 || binding.Spec.ValidationActions[0] != admissionv1.Deny || binding.Spec.MatchResources.NamespaceSelector.MatchLabels["spawnpod.dev/role"] != "workload" {
		t.Fatal("image policy or binding is not fail-closed")
	}
	manifest, err := Render(cfg)
	if err != nil || !strings.Contains(string(manifest), "kind: ValidatingAdmissionPolicyBinding") || !strings.Contains(string(manifest), "kind: ValidatingAdmissionPolicy") || !strings.Contains(string(manifest), "kind: LimitRange") || !strings.Contains(string(manifest), "kind: ResourceQuota") {
		t.Fatalf("invalid rendered manifest: %v", err)
	}
	limits, err := ContainerLimits(cfg)
	if err != nil || limits.Name != LimitRangeName || limits.Spec.Limits[0].Max.Cpu().String() != "1" {
		t.Fatalf("wrong container ceilings: %+v, %v", limits, err)
	}
	for name, max := range limits.Spec.Limits[0].Max {
		limit, hasLimit := limits.Spec.Limits[0].Default[name]
		request, hasRequest := limits.Spec.Limits[0].DefaultRequest[name]
		if !hasLimit || !hasRequest || limit.Cmp(max) != 0 || request.Cmp(max) != 0 {
			t.Fatalf("missing fail-closed default for %s", name)
		}
	}
	quota, err := NamespaceQuota(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pods := quota.Spec.Hard["count/pods"]
	configMaps := quota.Spec.Hard["count/configmaps"]
	policies := quota.Spec.Hard["count/networkpolicies.networking.k8s.io"]
	if quota.Name != QuotaName || pods.String() != "16" || configMaps.String() != "1024" || policies.String() != "33" {
		t.Fatalf("wrong namespace budget: %+v, %v", quota, err)
	}
	profile := cfg.Profiles["default"]
	profile.Image = "registry.example.com/other@sha256:" + strings.Repeat("e", 64)
	cfg.Profiles["default"] = profile
	changed, err := ImageExpression(cfg)
	if err != nil || changed == expression || strings.Contains(changed, "ci-base") {
		t.Fatal("config change did not change admission allowlist")
	}
	profile.Services = map[string]config.ServiceProfile{"redis": {
		Image:        "registry.example.com/redis@sha256:" + strings.Repeat("d", 64),
		Command:      []string{"redis-server", "--port", "6379"},
		Architecture: "amd64", UID: 10002, GID: 10002, Port: 6379,
		CPURequest: "50m", CPULimit: "250m", MemoryRequest: "64Mi", MemoryLimit: "256Mi",
		StorageRequest: "128Mi", StorageLimit: "512Mi",
	}}
	cfg.Profiles["default"] = profile
	withService, err := ImageExpression(cfg)
	if err != nil || !strings.Contains(withService, profile.Services["redis"].Image) || !strings.Contains(withService, "c.name == 'service' && c.image in") {
		t.Fatal("approved service image missing from admission allowlist")
	}
	profile.Services["redis"] = config.ServiceProfile{
		Image: profile.Services["redis"].Image, Command: []string{"redis-server"}, Architecture: "amd64", UID: 10002, GID: 10002, Port: 6379,
		CPURequest: "50m", CPULimit: "2", MemoryRequest: "64Mi", MemoryLimit: "2Gi", StorageRequest: "128Mi", StorageLimit: "8Gi",
	}
	cfg.Profiles["default"] = profile
	limits, err = ContainerLimits(cfg)
	if err != nil || limits.Spec.Limits[0].Max.Cpu().String() != "2" {
		t.Fatal("service ceiling was not included")
	}
}
