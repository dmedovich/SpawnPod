package kubernetes

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	agentPath        = "/opt/spawnpod/agent"
	containerName    = "job"
	ownerLabel       = "spawnpod.dev/managed"
	environmentLabel = "spawnpod.dev/environment"
	networkLabel     = "spawnpod.dev/network"
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// JobSpec is the approved engine contract. Kubernetes validates it again at
// this boundary before constructing any Pod.
type JobSpec = engine.JobSpec

func validateJobSpec(s JobSpec) error {
	if s.Network != "" && s.Network != "isolated" && s.Network != "source" {
		return errors.New("unsupported job network profile")
	}
	if !idPattern.MatchString(s.EnvironmentID) {
		return errors.New("environment ID must be 32 lowercase hex characters")
	}
	if !digestImage(s.Image) || !digestImage(s.AgentImage) {
		return errors.New("images must be pinned by sha256 digest")
	}
	if s.UID < 1 || s.GID < 1 {
		return errors.New("job UID/GID must be non-root")
	}
	if s.Architecture != "amd64" && s.Architecture != "arm64" {
		return errors.New("unsupported job architecture")
	}
	if len(s.ServiceAliases) > 8 {
		return errors.New("too many service aliases")
	}
	seenAliases := make(map[string]bool, len(s.ServiceAliases))
	for _, alias := range s.ServiceAliases {
		address, err := netip.ParseAddr(alias.IP)
		if !validServiceName(alias.Name) || seenAliases[alias.Name] || err != nil || !address.IsGlobalUnicast() {
			return errors.New("invalid service alias")
		}
		seenAliases[alias.Name] = true
	}
	for name, value := range map[string]string{"cpu request": s.CPURequest, "cpu limit": s.CPULimit, "memory request": s.MemoryRequest, "memory limit": s.MemoryLimit, "storage request": s.StorageRequest, "storage limit": s.StorageLimit, "workspace size": s.WorkspaceSize} {
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("invalid %s", name)
		}
	}
	cpuReq := resource.MustParse(s.CPURequest)
	cpuLim := resource.MustParse(s.CPULimit)
	memReq := resource.MustParse(s.MemoryRequest)
	memLim := resource.MustParse(s.MemoryLimit)
	storageReq := resource.MustParse(s.StorageRequest)
	storageLim := resource.MustParse(s.StorageLimit)
	workspace := resource.MustParse(s.WorkspaceSize)
	if cpuReq.Cmp(cpuLim) > 0 || memReq.Cmp(memLim) > 0 || storageReq.Cmp(storageLim) > 0 || workspace.Cmp(storageLim) > 0 {
		return errors.New("resource requests and workspace must fit limits")
	}
	return nil
}

func digestImage(image string) bool {
	before, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || before == "" || len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// BuildJobPod renders a restricted, single-container job Pod. The init image
// installs the agent binary into a shared volume without requiring a shell.
func BuildJobPod(s JobSpec) (*corev1.Pod, error) {
	if err := validateJobSpec(s); err != nil {
		return nil, err
	}
	falseValue := false
	trueValue := true
	uid := s.UID
	gid := s.GID
	seccomp := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	security := &corev1.SecurityContext{
		RunAsNonRoot: &trueValue, RunAsUser: &uid, RunAsGroup: &gid,
		AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue,
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: seccomp,
	}
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(s.CPURequest), corev1.ResourceMemory: resource.MustParse(s.MemoryRequest), corev1.ResourceEphemeralStorage: resource.MustParse(s.StorageRequest)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(s.CPULimit), corev1.ResourceMemory: resource.MustParse(s.MemoryLimit), corev1.ResourceEphemeralStorage: resource.MustParse(s.StorageLimit)},
	}
	aliases := append([]engine.ServiceAlias(nil), s.ServiceAliases...)
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Name < aliases[j].Name })
	hostAliases := make([]corev1.HostAlias, 0, len(aliases))
	for _, alias := range aliases {
		hostAliases = append(hostAliases, corev1.HostAlias{IP: alias.IP, Hostnames: []string{alias.Name}})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sp-" + s.EnvironmentID, Labels: map[string]string{ownerLabel: "true", environmentLabel: s.EnvironmentID, networkLabel: networkName(s.Network)}},
		Spec: corev1.PodSpec{
			HostAliases:                  hostAliases,
			NodeSelector:                 map[string]string{"kubernetes.io/arch": s.Architecture},
			EnableServiceLinks:           &falseValue,
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &falseValue,
			ServiceAccountName:           "spawnpod-job",
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: &trueValue, RunAsUser: &uid, RunAsGroup: &gid, FSGroup: &gid, SeccompProfile: seccomp},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: quantityPtr(s.WorkspaceSize)}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "tools", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			InitContainers: []corev1.Container{{
				Name: "install-agent", Image: s.AgentImage, ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/spawnpod-agent", "install", "/tools/agent"},
				VolumeMounts:    []corev1.VolumeMount{{Name: "tools", MountPath: "/tools"}},
				SecurityContext: security.DeepCopy(), Resources: resources,
			}},
			Containers: []corev1.Container{{
				Name: containerName, Image: s.Image, ImagePullPolicy: corev1.PullIfNotPresent,
				Command:    []string{agentPath, "serve"},
				WorkingDir: "/workspace",
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
					{Name: "tmp", MountPath: "/tmp"},
					{Name: "tools", MountPath: "/opt/spawnpod", ReadOnly: true},
				},
				SecurityContext: security.DeepCopy(), Resources: resources,
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{agentPath, "ping"}}}, InitialDelaySeconds: 1, PeriodSeconds: 2, TimeoutSeconds: 2, FailureThreshold: 3},
			}},
		},
	}
	return pod, nil
}

func quantityPtr(s string) *resource.Quantity { q := resource.MustParse(s); return &q }

func networkName(value string) string {
	if value == "" {
		return "isolated"
	}
	return value
}
