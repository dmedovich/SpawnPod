package installation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/controller"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
)

type Finding struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Detail   string `json:"detail"`
}
type Report struct {
	Revision string                `json:"revision"`
	Phase    string                `json:"phase"`
	Findings []Finding             `json:"findings"`
	Capacity []controller.Capacity `json:"capacity"`
	CIProven bool                  `json:"ciProven"`
}

func (r *Report) add(code string, err error) {
	if err != nil {
		r.Findings = append(r.Findings, Finding{"blocker", code, err.Error()})
	} else {
		r.Findings = append(r.Findings, Finding{"ok", code, "verified"})
	}
}
func (r Report) Err() error {
	for _, f := range r.Findings {
		if f.Severity == "blocker" {
			return errors.New("upgrade check failed; resolve blockers before proceeding")
		}
	}
	return nil
}

// Check uses GET/LIST and nonpersistent authorization reviews only. No apply,
// scale, drain, Secret copy or workload execution is performed.
func (b *Bundle) Check(ctx context.Context, client, controllerClient kubernetes.Interface, phase string) Report {
	r := Report{Revision: b.Index.Revision, Phase: phase, Findings: []Finding{}, Capacity: []controller.Capacity{}}
	if client == nil || controllerClient == nil {
		r.add("client", errors.New("operator and Controller clients are required"))
		return r
	}
	if phase != "switch" && phase != "ready" {
		r.add("phase", errors.New("phase must be switch or ready"))
		return r
	}
	server, err := client.Discovery().ServerVersion()
	if err == nil {
		v, e := version.ParseGeneric(server.GitVersion)
		err = e
		if e == nil && (v.Major() != 1 || v.Minor() < uint(b.Index.MinimumKubernetesMinor)) {
			err = errors.New("Kubernetes >=1.34 is required")
		}
	}
	r.add("kubernetes.version", err)
	r.add("controller.startup-and-prerequisites", controller.InstallationPrerequisites(ctx, b.Config, controllerClient))
	r.add("runner.secret-key", controller.InstallationRunnerSecret(ctx, b.Config, client))
	r.checkInventory(ctx, b, client, phase)
	capacity, err := controller.InstallationCapacity(ctx, b.Config, client)
	r.Capacity = capacity
	r.add("quota.observation", err)
	for _, item := range capacity {
		if item.Severity == "blocker" {
			r.add("quota.capacity", fmt.Errorf("%s %s: requested=%s used=%s hard=%s afterRecreate=%s", item.Quota, item.Resource, item.Requested, item.Used, item.Hard, item.AfterRecreate))
		} else if item.Severity == "warning" {
			r.Findings = append(r.Findings, Finding{"warning", "quota.recreate", fmt.Sprintf("%s %s fits after old Pods are removed; available=%s afterRecreate=%s requested=%s", item.Quota, item.Resource, item.Available, item.AfterRecreate, item.Requested)})
		}
	}
	workloads, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "spawnpod.dev/managed=true"})
	if err == nil && phase == "switch" && len(workloads.Items) > 0 {
		err = fmt.Errorf("%d managed workload Pods remain; keep current Controller running until cleanup finishes", len(workloads.Items))
	}
	if err == nil {
		for _, pod := range workloads.Items {
			if pod.Namespace != b.Config.Kubernetes.Namespace {
				err = errors.New("managed workloads exist outside this installation namespace; global admission is shared, review other installations before proceeding")
			}
		}
	}
	r.add("workloads.drained", err)
	d, err := client.AppsV1().Deployments(b.Config.Kubernetes.LeaseNamespace).Get(ctx, "spawnpod", metav1.GetOptions{})
	if err == nil {
		if phase == "switch" {
			if d.Spec.Replicas == nil || *d.Spec.Replicas != 0 || d.Status.ObservedGeneration < d.Generation || d.Status.Replicas != 0 {
				err = errors.New("installation must be scaled to zero and its generation observed before changing policy/config")
			}
		}
		if phase == "ready" {
			if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 || d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas != 1 || d.Status.AvailableReplicas != 1 || d.Status.ReadyReplicas != 1 || d.Status.Replicas != 1 {
				err = errors.New("target generation is not fully available (require exactly one ready updated replica)")
			}
		}
	}
	r.add("deployment."+phase, err)
	pods, err := client.CoreV1().Pods(b.Config.Kubernetes.LeaseNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=spawnpod"})
	if err == nil {
		if phase == "switch" && len(pods.Items) != 0 {
			err = errors.New("old installation Pods still exist; wait for full deletion and quota accounting")
		}
		if phase == "ready" {
			if len(pods.Items) != 1 {
				err = errors.New("expected exactly one installation Pod")
			} else {
				p := pods.Items[0]
				if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning || len(p.Status.ContainerStatuses) != 2 {
					err = errors.New("installation Pod is not Running with both container statuses")
				}
				for _, c := range p.Status.ContainerStatuses {
					if !c.Ready || c.State.Running == nil {
						err = fmt.Errorf("container %s is not Ready and Running", c.Name)
					}
				}
				if len(p.Spec.Containers) != 2 || p.Spec.Containers[0].Image != b.Index.Images[0].Reference || p.Spec.Containers[1].Image != b.Index.Images[1].Reference {
					err = errors.New("running Pod images differ from target bundle")
				}
			}
		}
	}
	r.add("installation.pods", err)
	if phase == "ready" {
		var doctor bytes.Buffer
		err := controller.DoctorWithOptions(ctx, b.Config, client, &doctor, controller.DoctorOptions{Installation: true})
		if err != nil {
			err = fmt.Errorf("%w\n%s", err, doctor.String())
		}
		r.add("installation.doctor", err)
		if d != nil {
			r.add("deployment.managed-fields", b.checkDeployment(d))
		}
	}
	r.checkImages(ctx, b, client)
	r.Findings = append(r.Findings, Finding{"warning", "forgejo.smoke-required", "Read-only checks do not prove Runner registration or CI execution. Run the real Forgejo smoke workflow and verify cleanup before declaring COMPLETE."})
	return r
}

func isLocal(image string) bool {
	host := strings.SplitN(image, "/", 2)[0]
	return host == "localhost" || strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "[::1]")
}
func (r *Report) checkImages(ctx context.Context, b *Bundle, client kubernetes.Interface) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		r.add("images.nodes", err)
		return
	}
	ready := []corev1.Node{}
	for _, n := range nodes.Items {
		excluded := n.Spec.Unschedulable
		for _, taint := range n.Spec.Taints {
			if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
				excluded = true
			}
		}
		if excluded {
			continue
		}
		healthy := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				healthy = true
			}
		}
		if healthy {
			ready = append(ready, n)
		}
	}
	if len(ready) == 0 {
		r.add("images.nodes", errors.New("no Ready schedulable nodes"))
		return
	}
	// Reference Pods have no node selector. Check every Ready schedulable node
	// matching the configured workload architecture and tolerable taints.
	for _, image := range b.Index.Images {
		eligible := 0
		for _, n := range ready {
			arch := n.Status.NodeInfo.Architecture
			if image.Role == "controller+agent" && arch != "amd64" && arch != "arm64" {
				r.add("images.runtime-platform", fmt.Errorf("node %s has unsupported SpawnPod runtime architecture %s", n.Name, arch))
				continue
			}
			if image.Architecture != "" && arch != image.Architecture {
				continue
			}
			eligible++
			found := false
			for _, cached := range n.Status.Images {
				for _, name := range cached.Names {
					if name == image.Reference {
						found = true
					}
				}
			}
			if !found {
				severity := "warning"
				if isLocal(image.Reference) {
					severity = "blocker"
				}
				r.Findings = append(r.Findings, Finding{severity, "images.cache", fmt.Sprintf("%s on node %s (%s): pinned digest not confirmed by Node.status.images; verify registry platform support/pull or inspect containerd on this node. Node cache observations may be truncated", image.Role, n.Name, arch)})
			}
		}
		if eligible == 0 {
			r.add("images.architecture", fmt.Errorf("%s: no eligible node for architecture %s", image.Role, image.Architecture))
		}
	}
}

// Compare the full target Deployment spec with enumerated API defaults.
func (b *Bundle) checkDeployment(actual any) error {
	objects, err := DecodeObjects(b.Files["30-deployment.yaml"])
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if obj.GetKind() != "Deployment" {
			continue
		}
		encoded, _ := json.Marshal(actual)
		var live map[string]any
		_ = json.Unmarshal(encoded, &live)
		if err := compareDeployment(live["spec"], obj.Object["spec"]); err != nil {
			return err
		}
	}
	return nil
}
func subsetDifference(path string, actual, expected any) string {
	// JSON conversion gives identical numeric types for desired and typed live data.
	a, _ := json.Marshal(actual)
	e, _ := json.Marshal(expected)
	var av, ev any
	_ = json.Unmarshal(a, &av)
	_ = json.Unmarshal(e, &ev)
	return subset(path, av, ev)
}
func subset(path string, a, e any) string {
	if em, ok := e.(map[string]any); ok {
		am, ok := a.(map[string]any)
		if !ok {
			return path
		}
		keys := make([]string, 0, len(em))
		for k := range em {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if d := subset(path+"."+k, am[k], em[k]); d != "" {
				return d
			}
		}
		return ""
	}
	if es, ok := e.([]any); ok {
		as, ok := a.([]any)
		if !ok || len(as) != len(es) {
			return path
		}
		for i := range es {
			if d := subset(fmt.Sprintf("%s[%d]", path, i), as[i], es[i]); d != "" {
				return d
			}
		}
		return ""
	}
	if !reflect.DeepEqual(a, e) {
		return path
	}
	return ""
}

// MountedConfig gives discovery callers the same effective config that startup
// reads, without exposing any Runner Secret content.
func MountedConfig(ctx context.Context, client kubernetes.Interface, namespace string) (config.Config, error) {
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, "spawnpod-config", metav1.GetOptions{})
	if err != nil {
		return config.Config{}, err
	}
	cfg, err := config.Decode(strings.NewReader(cm.Data["config.yaml"]))
	if err != nil {
		return config.Config{}, errors.New("installed config.yaml is invalid (values withheld)")
	}
	return cfg, nil
}
