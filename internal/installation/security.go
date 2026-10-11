package installation

import (
	"bytes"

	"github.com/dmedovich/spawnpod/deploy"
	"github.com/dmedovich/spawnpod/internal/config"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func security(cfg config.Config) ([]byte, []byte, error) {
	objects, err := DecodeObjects(deploy.Security)
	if err != nil {
		return nil, nil, err
	}
	workload, infra := cfg.Kubernetes.Namespace, cfg.Kubernetes.LeaseNamespace
	readRole := "spawnpod-admission-read"
	if workload != "ci" || infra != "infra" {
		readRole = workload + "-admission-read"
	}
	translate := func(s string) string {
		switch s {
		case "ci":
			return workload
		case "infra":
			return infra
		case "spawnpod-admission-read":
			return readRole
		}
		return s
	}
	// Translate only reference identity fields, never CEL or arbitrary YAML text.
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, value := range x {
				if k == "namespace" || k == "name" {
					if s, ok := value.(string); ok {
						x[k] = translate(s)
					}
				}
				if k == "resourceNames" {
					if a, ok := value.([]any); ok {
						for i, s := range a {
							if s == "ci" {
								a[i] = workload
							}
						}
					}
				}
				walk(value)
			}
		case []any:
			for _, value := range x {
				walk(value)
			}
		}
	}
	var out bytes.Buffer
	for i := range objects {
		o := &objects[i]
		walk(o.Object)
		if o.GetKind() == "Role" && o.GetName() == "spawnpod-lease" {
			rules := o.Object["rules"].([]any)
			rules[1].(map[string]any)["resourceNames"] = []any{cfg.Kubernetes.LeaseName}
		}
		data, err := yaml.Marshal(o.Object)
		if err != nil {
			return nil, nil, err
		}
		if i > 0 {
			out.WriteString("---\n")
		}
		out.Write(data)
	}
	deny := provider.BuildDefaultDenyPolicy()
	deny.Namespace = workload
	deny.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}
	network, err := yaml.Marshal(deny)
	return out.Bytes(), network, err
}
