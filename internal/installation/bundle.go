// Package installation prepares a deterministic, declarative installation.
// It never applies resources to Kubernetes and never copies Runner credentials.
package installation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/config"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const IndexName = "spawnpod-bundle.json"

type Object struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	File       string `json:"file"`
	Phase      string `json:"phase"`
}
type File struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}
type Phase struct {
	Name         string   `json:"name"`
	Requires     []string `json:"requires"`
	Instructions string   `json:"instructions"`
}
type Image struct {
	Reference    string `json:"reference"`
	Role         string `json:"role"`
	Architecture string `json:"architecture,omitempty"`
}
type Index struct {
	Schema                 int      `json:"schema"`
	Target                 string   `json:"target"`
	Revision               string   `json:"revision"`
	ConfigSHA256           string   `json:"effectiveConfigSHA256"`
	MinimumKubernetesMinor int      `json:"minimumKubernetesMinor"`
	Images                 []Image  `json:"images"`
	Files                  []File   `json:"files"`
	Inventory              []Object `json:"inventory"`
	Phases                 []Phase  `json:"phases"`
}
type Bundle struct {
	Config config.Config
	Index  Index
	Files  map[string][]byte
}

// Build uses a single runtime image for Controller and Agent. Source config is
// copied; the operator's source file and profile maps are never modified.
func Build(source config.Config, target, runtimeImage, runnerImage string) (*Bundle, error) {
	if target == "" || strings.ContainsAny(target, "\r\n\x00") {
		return nil, errors.New("target release is required")
	}
	if source.Kubernetes.Namespace == source.Kubernetes.LeaseNamespace {
		return nil, errors.New("installation requires separate workload and Controller namespaces")
	}
	effective, err := config.InstallationConfig(source)
	if err != nil {
		return nil, err
	}
	effective.AgentImage = runtimeImage
	deploymentYAML, err := RenderDeployment(effective, runtimeImage, runnerImage)
	if err != nil {
		return nil, err
	}
	admissionYAML, err := RenderAdmission(effective)
	if err != nil {
		return nil, err
	}
	configBytes, err := config.InstallationBytes(effective)
	if err != nil {
		return nil, err
	}
	securityYAML, networkYAML, err := security(effective)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Config: effective, Files: map[string][]byte{
		"00-security.yaml":   securityYAML,
		"10-network.yaml":    networkYAML,
		"20-admission.yaml":  admissionYAML,
		"30-deployment.yaml": deploymentYAML,
	}}
	b.Index = Index{Schema: 1, Target: target, MinimumKubernetesMinor: 34, ConfigSHA256: checksum(configBytes), Phases: upgradePhases()}
	b.Index.Images = []Image{{Reference: runtimeImage, Role: "controller+agent"}, {Reference: runnerImage, Role: "runner"}}
	names := make([]string, 0, len(effective.Profiles))
	for name := range effective.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := effective.Profiles[name]
		b.Index.Images = append(b.Index.Images, Image{Reference: p.Image, Role: "profile/" + name, Architecture: p.Architecture})
		services := make([]string, 0, len(p.Services))
		for s := range p.Services {
			services = append(services, s)
		}
		sort.Strings(services)
		for _, s := range services {
			v := p.Services[s]
			b.Index.Images = append(b.Index.Images, Image{Reference: v.Image, Role: "profile/" + name + "/service/" + s, Architecture: v.Architecture})
		}
	}
	for _, name := range manifestNames() {
		b.Index.Files = append(b.Index.Files, File{Name: name, SHA256: checksum(b.Files[name])})
		objs, err := DecodeObjects(b.Files[name])
		if err != nil {
			return nil, err
		}
		phase := "APPLY_DEPENDENCIES"
		if name == "20-admission.yaml" {
			phase = "POLICY"
		}
		if name == "30-deployment.yaml" {
			phase = "SWITCH_CONTROLLER"
		}
		for _, obj := range objs {
			b.Index.Inventory = append(b.Index.Inventory, Object{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Namespace: obj.GetNamespace(), Name: obj.GetName(), File: name, Phase: phase})
		}
	}
	revisionBytes, err := json.Marshal(b.Index)
	if err != nil {
		return nil, err
	}
	b.Index.Revision = checksum(revisionBytes)
	indexBytes, err := json.MarshalIndent(b.Index, "", "  ")
	if err != nil {
		return nil, err
	}
	b.Files[IndexName] = append(indexBytes, '\n')
	return b, nil
}
func checksum(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func manifestNames() []string {
	return []string{"00-security.yaml", "10-network.yaml", "20-admission.yaml", "30-deployment.yaml"}
}
func DecodeObjects(data []byte) ([]unstructured.Unstructured, error) {
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []unstructured.Unstructured
	for {
		var o unstructured.Unstructured
		err := decoder.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(o.Object) != 0 {
			if o.GetKind() == "" || o.GetName() == "" {
				return nil, errors.New("manifest requires kind and metadata.name")
			}
			objects = append(objects, o)
		}
	}
	return objects, nil
}
func upgradePhases() []Phase {
	return []Phase{
		{Name: "DISCOVER", Requires: []string{}, Instructions: "Identify current binary, runtime digest, mounted config and GitOps revision. Run doctor --installation with current config. A history archive is not a rollback bundle."},
		{Name: "BACKUP", Requires: []string{"DISCOVER"}, Instructions: "Keep a verified complete previous generated bundle and source config in a private GitOps revision. Verify its hashes. Keep the existing Runner Secret in place; never export credentials."},
		{Name: "DRAIN", Requires: []string{"BACKUP"}, Instructions: "Pause Forgejo job submissions and wait for every managed workload Pod to finish and clean up. Do not delete active Pods or tombstones. Keep the current Controller running for cleanup."},
		{Name: "STOP_CONTROLLER", Requires: []string{"DRAIN"}, Instructions: "Suspend automatic GitOps reconciliation for the Deployment; scale infra/spawnpod to zero and wait until every installation Pod is gone. This also stops Runner. Do not change config/admission before this gate."},
		{Name: "APPLY_DEPENDENCIES", Requires: []string{"STOP_CONTROLLER"}, Instructions: "Apply 00-security.yaml and 10-network.yaml explicitly. Existing namespace contents and Runner Secret are retained."},
		{Name: "POLICY", Requires: []string{"APPLY_DEPENDENCIES"}, Instructions: "Apply 20-admission.yaml while Controller is stopped and workloads are drained. It allows only the target Agent digest. Do not use expanded admission with rc.4/rc.5 startup."},
		{Name: "PREFLIGHT", Requires: []string{"POLICY"}, Instructions: "Run upgrade check --phase switch against this bundle. All blockers and unverified image prerequisites must be resolved before switching."},
		{Name: "SWITCH_CONTROLLER", Requires: []string{"PREFLIGHT"}, Instructions: "Apply 30-deployment.yaml as one phase (ConfigMap, Deployment and monitor Service); resume GitOps only against this complete target revision. Application is not atomic."},
		{Name: "WAIT_READY", Requires: []string{"SWITCH_CONTROLLER"}, Instructions: "Run upgrade check --phase ready. Require observed target generation, one available replica, every container Ready, target images and mounted config. CrashLoopBackOff is failure."},
		{Name: "SMOKE_TEST", Requires: []string{"WAIT_READY"}, Instructions: "Resume a controlled Forgejo workflow with runs-on: spawnpod. Verify real execution, completion and cleanup. Pod readiness alone does not prove registration or a working CI pipeline."},
		{Name: "COMPLETE", Requires: []string{"SMOKE_TEST"}, Instructions: "Resume submissions only after successful Forgejo smoke. Preserve the previous complete bundle. On failure stop submissions, drain, stop Controller, restore previous dependencies/admission first and previous deployment/config second, then repeat ready and smoke gates. Never use Deployment-only rollout undo."},
	}
}
