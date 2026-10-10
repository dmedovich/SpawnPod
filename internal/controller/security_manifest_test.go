package controller

import (
	"errors"
	"io"
	"os"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

func TestSecurityManifestKeepsWorkloadPermissionsScoped(t *testing.T) {
	file, err := os.Open("../../deploy/security.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	seen := map[string]bool{}
	for {
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		kind, _ := doc["kind"].(string)
		metadata, _ := doc["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		seen[kind+"/"+name] = true
		if kind == "Role" || kind == "ClusterRole" {
			if metadata["namespace"] != "ci" && metadata["namespace"] != "infra" {
				if kind == "Role" {
					t.Fatal("Role is outside the two approved namespaces")
				}
			}
			for _, rawRule := range doc["rules"].([]any) {
				rule := rawRule.(map[string]any)
				for _, rawResource := range rule["resources"].([]any) {
					resource := rawResource.(string)
					if resource == "*" || resource == "secrets" || resource == "roles" || resource == "rolebindings" {
						t.Fatalf("unsafe Role resource %q", resource)
					}
					if resource == "serviceaccounts" && (kind != "Role" || name != "spawnpod-workloads" || len(rule["resourceNames"].([]any)) != 1 || rule["resourceNames"].([]any)[0] != "spawnpod-job") {
						t.Fatal("ServiceAccount read must be limited to spawnpod-job")
					}
				}
				for _, rawVerb := range rule["verbs"].([]any) {
					if rawVerb == "*" || rawVerb == "impersonate" || rawVerb == "escalate" || rawVerb == "bind" {
						t.Fatalf("unsafe Role verb %q", rawVerb)
					}
				}
				if kind == "ClusterRole" {
					resources := rule["resources"].([]any)
					wanted := "spawnpod-workload"
					if len(resources) == 1 && resources[0] == "namespaces" {
						wanted = "ci"
					}
					if len(rule["verbs"].([]any)) != 1 || rule["verbs"].([]any)[0] != "get" || len(rule["resourceNames"].([]any)) != 1 || rule["resourceNames"].([]any)[0] != wanted {
						t.Fatal("admission ClusterRole is broader than named read access")
					}
				}
			}
		}
		if kind == "Namespace" && name == "ci" {
			labels := metadata["labels"].(map[string]any)
			if labels["pod-security.kubernetes.io/enforce"] != "restricted" || labels["spawnpod.dev/role"] != "workload" || labels["pod-security.kubernetes.io/enforce-version"] == "latest" {
				t.Fatal("ci namespace lacks pinned Restricted admission")
			}
		}
	}
	for _, key := range []string{"Namespace/infra", "Namespace/ci", "ServiceAccount/spawnpod-controller", "ServiceAccount/spawnpod-job", "Role/spawnpod-workloads", "Role/spawnpod-lease", "RoleBinding/spawnpod-workloads", "RoleBinding/spawnpod-lease", "ClusterRole/spawnpod-admission-read", "ClusterRoleBinding/spawnpod-admission-read"} {
		if !seen[key] {
			t.Fatalf("missing manifest object %s", key)
		}
	}
}
