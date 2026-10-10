// Package deployment renders the single-controller installation topology.
package deployment

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/dmedovich/spawnpod/internal/config"
	yaml "go.yaml.in/yaml/v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	syaml "sigs.k8s.io/yaml"
)

var pinned = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func ptr[T any](v T) *T { return &v }

// Render omits host kubeconfig paths and references an operator-created Secret.
// Only the controller receives a projected Kubernetes token; Runner does not.
func Render(cfg config.Config, controllerImage, runnerImage string) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if !pinned.MatchString(controllerImage) || !pinned.MatchString(runnerImage) {
		return nil, errors.New("controller and Runner images must be digest-pinned")
	}
	if cfg.Kubernetes.Namespace != "ci" || cfg.Kubernetes.LeaseNamespace != "infra" || cfg.Kubernetes.LeaseName != "spawnpod-controller" {
		return nil, errors.New("installation renderer requires the infra/ci reference RBAC layout")
	}
	cfg.Kubernetes.Kubeconfig = ""
	cfg.Controller.SocketPath = "/run/spawnpod/private/backend.sock"
	cfg.Controller.MonitorAddress = ":9090"
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	content, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-config", Namespace: "infra"}, Data: map[string]string{"config.yaml": string(content)}}
	security := &corev1.SecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(10001)), RunAsGroup: ptr(int64(10001)), ReadOnlyRootFilesystem: ptr(true), AllowPrivilegeEscalation: ptr(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	resources := func(cpu, mem string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}}
	}
	socketMount := corev1.VolumeMount{Name: "socket", MountPath: "/run/spawnpod"}
	controller := corev1.Container{Name: "controller", Image: controllerImage, Command: []string{"/spawnpod"}, Args: []string{"serve", "--config", "/etc/spawnpod/config.yaml"}, SecurityContext: security.DeepCopy(), Resources: resources("100m", "128Mi"), Ports: []corev1.ContainerPort{{Name: "monitor", ContainerPort: 9090}}, VolumeMounts: []corev1.VolumeMount{socketMount, {Name: "config", MountPath: "/etc/spawnpod", ReadOnly: true}, {Name: "api", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}, {Name: "controller-tmp", MountPath: "/tmp"}}}
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromString("monitor")}}, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 3}
	}
	controller.LivenessProbe = probe("/healthz")
	controller.ReadinessProbe = probe("/readyz")
	controller.StartupProbe = probe("/healthz")
	controller.StartupProbe.FailureThreshold = 24
	runner := corev1.Container{Name: "runner", Image: runnerImage, Command: []string{"/bin/sh"}, Args: []string{"-ec", "until test -S /run/spawnpod/private/backend.sock; do sleep 1; done; exec /bin/forgejo-runner --config /etc/runner/runner.yaml daemon"}, SecurityContext: security.DeepCopy(), Resources: resources("100m", "128Mi"), Env: []corev1.EnvVar{{Name: "HOME", Value: "/home/runner"}}, VolumeMounts: []corev1.VolumeMount{socketMount, {Name: "runner", MountPath: "/etc/runner", ReadOnly: true}, {Name: "runner-home", MountPath: "/home/runner"}, {Name: "runner-tmp", MountPath: "/tmp"}}}
	empty := func(name string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr(resource.MustParse("1Gi"))}}}
	}
	volumes := []corev1.Volume{
		empty("socket"), empty("controller-tmp"), empty("runner-tmp"), empty("runner-home"),
		{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "spawnpod-config"}}}},
		{Name: "runner", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "spawnpod-runner", DefaultMode: ptr(int32(0440))}}},
		{Name: "api", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr(int32(0440)), Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: ptr(int64(3600))}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"}}}}},
			},
		}}},
	}
	drain, _ := cfg.DrainTimeout()
	cleanup, _ := cfg.CleanupTimeout()
	labels := map[string]string{"app.kubernetes.io/name": "spawnpod"}
	deploy := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: "spawnpod", Namespace: "infra"}, Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(1)), RevisionHistoryLimit: ptr(int32(2)), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{"prometheus.io/scrape": "true", "prometheus.io/port": "9090", "prometheus.io/path": "/metrics", "checksum/config": fmt.Sprintf("%x", sha256.Sum256(content))}}, Spec: corev1.PodSpec{ServiceAccountName: "spawnpod-controller", AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false), TerminationGracePeriodSeconds: ptr(int64((drain + cleanup + 20*time.Second) / time.Second)), SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(10001)), RunAsGroup: ptr(int64(10001)), FSGroup: ptr(int64(10001)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}, Containers: []corev1.Container{controller, runner}, Volumes: volumes}}}}
	service := &corev1.Service{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-monitor", Namespace: "infra"}, Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, Selector: labels, Ports: []corev1.ServicePort{{Name: "monitor", Port: 9090, TargetPort: intstr.FromString("monitor")}}}}
	var out bytes.Buffer
	fmt.Fprintf(&out, "# Generated installation; %s-second termination budget.\n", strconv.FormatInt(*deploy.Spec.Template.Spec.TerminationGracePeriodSeconds, 10))
	for index, obj := range []any{cm, deploy, service} {
		b, err := syaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		if index > 0 {
			out.WriteString("---\n")
		}
		out.Write(b)
	}
	return out.Bytes(), nil
}
