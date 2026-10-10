package config

import (
	"os"
	"strings"
	"testing"
)

func example(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestExampleAndStrictDecode(t *testing.T) {
	base := example(t)
	cfg, err := Decode(strings.NewReader(base))
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.Kubernetes.Namespace != "ci" || len(cfg.Profiles) != 1 {
		t.Fatalf("wrong decoded config: %+v", cfg)
	}
	for _, tc := range []struct{ name, content string }{
		{"unknown field", base + "unexpected: true\n"},
		{"second document", base + "---\nversion: 1\n"},
		{"unpinned image", strings.Replace(base, "@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ":latest", 1)},
		{"root user", strings.Replace(base, "uid: 10001", "uid: 0", 1)},
		{"limit below request", strings.Replace(base, "cpuLimit: \"1\"", "cpuLimit: 100m", 1)},
		{"invalid label", strings.Replace(base, "leaseName: spawnpod-controller", "leaseName: Invalid_Name", 1)},
		{"invalid duration", strings.Replace(base, "maxLifetime: 1h", "maxLifetime: 25h", 1)},
		{"invalid architecture", strings.Replace(base, "architecture: amd64", "architecture: mips64", 1)},
		{"unsafe socket path", strings.Replace(base, "socketPath: /run/spawnpod/backend.sock", "socketPath: relative.sock", 1)},
		{"missing pod quota", strings.Replace(base, "maxPods: 16", "maxPods: 0", 1)},
		{"quota request exceeds limit", strings.Replace(base, "requestsCPU: \"4\"", "requestsCPU: \"9\"", 1)},
		{"quota too small for job", strings.Replace(base, "limitsStorage: 48Gi", "limitsStorage: 1Gi", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(tc.content)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestServiceProfileValidation(t *testing.T) {
	base := strings.Replace(example(t), "    maxLifetime: 1h\n", `    maxLifetime: 1h
    services:
      redis:
        image: registry.example.com/redis@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
        command: [redis-server, --port, "6379"]
        architecture: amd64
        uid: 10002
        gid: 10002
        port: 6379
        cpuRequest: 50m
        cpuLimit: 250m
        memoryRequest: 64Mi
        memoryLimit: 256Mi
        storageRequest: 128Mi
        storageLimit: 512Mi
`, 1)
	cfg, err := Decode(strings.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Profiles["default"].Services["redis"].Port != 6379 {
		t.Fatal("service profile was not loaded")
	}
	for _, tc := range []struct{ name, content string }{
		{"mutable image", strings.Replace(base, "@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", ":latest", 1)},
		{"different architecture", strings.Replace(base, "        architecture: amd64", "        architecture: arm64", 1)},
		{"unknown field", strings.Replace(base, "        port: 6379", "        unknown: value\n        port: 6379", 1)},
		{"service exceeds pod quota", strings.Replace(base, "maxPods: 16", "maxPods: 1", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(tc.content)); err == nil {
				t.Fatal("invalid service profile accepted")
			}
		})
	}
}

func TestSourceProfileRequiresPinnedProxy(t *testing.T) {
	base := strings.Replace(example(t), "profiles:\n  default:\n", "sourceProxy: http://10.42.0.1:3128\nprofiles:\n  default:\n    network: source\n", 1)
	cfg, err := Decode(strings.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	ip, port, err := cfg.SourceProxyEndpoint()
	if err != nil || ip.String() != "10.42.0.1" || port != 3128 {
		t.Fatalf("proxy endpoint %s:%d: %v", ip, port, err)
	}
	for _, content := range []string{
		strings.Replace(base, "sourceProxy: http://10.42.0.1:3128\n", "", 1),
		strings.Replace(base, "10.42.0.1", "proxy.example.com", 1),
		strings.Replace(base, "network: source", "network: internet", 1),
		strings.Replace(base, "maxNetworkPolicies: 33", "maxNetworkPolicies: 1", 1),
	} {
		if _, err := Decode(strings.NewReader(content)); err == nil {
			t.Fatal("invalid source configuration accepted")
		}
	}
}
