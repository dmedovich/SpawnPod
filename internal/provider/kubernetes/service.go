package kubernetes

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/dmedovich/spawnpod/internal/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const serviceLabel = "spawnpod.dev/service"

var serviceNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ServiceSpec keeps the provider boundary aligned with the engine's approved
// non-secret snapshot.
type ServiceSpec = engine.ServiceSpec

func validateServiceSpec(s ServiceSpec) (corev1.ResourceRequirements, error) {
	if !idPattern.MatchString(s.EnvironmentID) {
		return corev1.ResourceRequirements{}, errors.New("invalid service environment ID")
	}
	if !validServiceName(s.Name) {
		return corev1.ResourceRequirements{}, errors.New("invalid service name")
	}
	if !digestImage(s.Image) || !digestImage(s.AgentImage) {
		return corev1.ResourceRequirements{}, errors.New("service and agent images must be pinned by sha256 digest")
	}
	if len(s.Command) == 0 || len(s.Command) > 128 || s.Command[0] == "" {
		return corev1.ResourceRequirements{}, errors.New("approved service command is required")
	}
	commandBytes := 0
	for _, arg := range s.Command {
		commandBytes += len(arg)
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return corev1.ResourceRequirements{}, errors.New("invalid service command")
		}
	}
	if commandBytes > 256<<10 {
		return corev1.ResourceRequirements{}, errors.New("service command is too large")
	}
	if s.Architecture != "amd64" && s.Architecture != "arm64" {
		return corev1.ResourceRequirements{}, errors.New("unsupported service architecture")
	}
	if s.UID < 1 || s.GID < 1 || s.Port < 1 || s.Port > 65535 {
		return corev1.ResourceRequirements{}, errors.New("invalid service identity or port")
	}
	values := map[string]string{
		"cpu request": s.CPURequest, "cpu limit": s.CPULimit,
		"memory request": s.MemoryRequest, "memory limit": s.MemoryLimit,
		"storage request": s.StorageRequest, "storage limit": s.StorageLimit,
	}
	parsed := make(map[string]resource.Quantity, len(values))
	for key, value := range values {
		quantity, err := resource.ParseQuantity(value)
		if err != nil || quantity.Sign() <= 0 {
			return corev1.ResourceRequirements{}, fmt.Errorf("invalid service %s", key)
		}
		parsed[key] = quantity
	}
	for _, pair := range [][2]string{{"cpu request", "cpu limit"}, {"memory request", "memory limit"}, {"storage request", "storage limit"}} {
		request, limit := parsed[pair[0]], parsed[pair[1]]
		if request.Cmp(limit) > 0 {
			return corev1.ResourceRequirements{}, fmt.Errorf("service %s exceeds %s", pair[0], pair[1])
		}
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: parsed["cpu request"], corev1.ResourceMemory: parsed["memory request"], corev1.ResourceEphemeralStorage: parsed["storage request"]},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: parsed["cpu limit"], corev1.ResourceMemory: parsed["memory limit"], corev1.ResourceEphemeralStorage: parsed["storage limit"]},
	}, nil
}

func validServiceName(name string) bool {
	if len(name) == 0 || len(name) > 27 || name == "localhost" || name == "kubernetes" || !serviceNamePattern.MatchString(name) {
		return false
	}
	return true
}

func serviceResourceName(s ServiceSpec) string { return "sp-" + s.EnvironmentID + "-" + s.Name }

func serviceLabels(s ServiceSpec) map[string]string {
	return map[string]string{ownerLabel: "true", environmentLabel: s.EnvironmentID, serviceLabel: s.Name}
}

// BuildServicePod runs the agent as PID 1. The approved service command and
// workflow environment arrive later over pods/exec stdin, never in PodSpec.
func BuildServicePod(s ServiceSpec) (*corev1.Pod, error) {
	resources, err := validateServiceSpec(s)
	if err != nil {
		return nil, err
	}
	falseValue, trueValue := false, true
	uid, gid := s.UID, s.GID
	port := s.Port
	labels := serviceLabels(s)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: serviceResourceName(s), Labels: labels},
		Spec: corev1.PodSpec{
			NodeSelector:                 map[string]string{"kubernetes.io/arch": s.Architecture},
			EnableServiceLinks:           &falseValue,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &falseValue,
			ServiceAccountName:           "spawnpod-job",
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: &trueValue, RunAsUser: &uid, RunAsGroup: &gid, FSGroup: &gid, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Volumes: []corev1.Volume{
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tools", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			InitContainers: []corev1.Container{{
				Name: "install-agent", Image: s.AgentImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command:      []string{"/spawnpod-agent", "install", "/tools/agent"},
				VolumeMounts: []corev1.VolumeMount{{Name: "tools", MountPath: "/tools"}},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: &trueValue, RunAsUser: &uid, RunAsGroup: &gid,
					AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue,
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Resources: resources,
			}},
			Containers: []corev1.Container{{
				Name: "service", Image: s.Image, ImagePullPolicy: corev1.PullIfNotPresent,
				Command:      []string{agentPath, "serve-service"},
				Ports:        []corev1.ContainerPort{{Name: "service", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
				VolumeMounts: []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "tools", MountPath: "/opt/spawnpod", ReadOnly: true}},
				Resources:    resources,
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot: &trueValue, RunAsUser: &uid, RunAsGroup: &gid,
					AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue,
					Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)}}, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 3},
			}},
		},
	}, nil
}

// BuildService renders a private ClusterIP selector for this environment's
// service Pod. No NodePort, host port or LoadBalancer is exposed.
func BuildService(s ServiceSpec) (*corev1.Service, error) {
	if _, err := validateServiceSpec(s); err != nil {
		return nil, err
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceResourceName(s), Labels: serviceLabels(s)},
		Spec: corev1.ServiceSpec{
			Type:            corev1.ServiceTypeClusterIP,
			Selector:        serviceLabels(s),
			Ports:           []corev1.ServicePort{{Name: "service", Protocol: corev1.ProtocolTCP, Port: s.Port, TargetPort: intstr.FromInt32(s.Port)}},
			SessionAffinity: corev1.ServiceAffinityNone,
		},
	}, nil
}
