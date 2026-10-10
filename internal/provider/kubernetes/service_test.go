package kubernetes

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func validServiceSpec() ServiceSpec {
	return ServiceSpec{
		EnvironmentID: strings.Repeat("a", 32), Name: "redis",
		Image:        "registry.example/redis@sha256:" + strings.Repeat("d", 64),
		AgentImage:   "registry.example/agent@sha256:" + strings.Repeat("c", 64),
		Command:      []string{"redis-server", "--port", "6379"},
		Architecture: "amd64", UID: 10001, GID: 10001, Port: 6379,
		CPURequest: "50m", CPULimit: "250m", MemoryRequest: "64Mi", MemoryLimit: "256Mi",
		StorageRequest: "128Mi", StorageLimit: "512Mi",
	}
}

func TestBuildServiceResourcesArePrivateAndRestricted(t *testing.T) {
	spec := validServiceSpec()
	pod, err := BuildServicePod(spec)
	if err != nil {
		t.Fatal(err)
	}
	service, err := BuildService(spec)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Name != service.Name || service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Selector[serviceLabel] != spec.Name {
		t.Fatal("service does not select only its matching Pod")
	}
	if pod.Spec.NodeSelector["kubernetes.io/arch"] != spec.Architecture || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("service Pod has wrong architecture or Kubernetes credentials")
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.Containers[0].Env) != 0 || len(pod.Spec.Containers[0].EnvFrom) != 0 {
		t.Fatal("workflow environment entered the service PodSpec")
	}
	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers[0].Command) != 2 || pod.Spec.Containers[0].Command[0] != agentPath || pod.Spec.Containers[0].Command[1] != "serve-service" {
		t.Fatal("service does not start under the agent")
	}
	security := pod.Spec.Containers[0].SecurityContext
	if security == nil || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatal("service container is not restricted")
	}
	if pod.Spec.Containers[0].ReadinessProbe == nil || pod.Spec.Containers[0].ReadinessProbe.TCPSocket == nil || pod.Spec.Containers[0].ReadinessProbe.TCPSocket.Port.IntVal != spec.Port {
		t.Fatal("service readiness does not probe the approved port")
	}
	if service.Spec.Ports[0].Port != spec.Port || service.Spec.Ports[0].NodePort != 0 || pod.Spec.Containers[0].Ports[0].HostPort != 0 {
		t.Fatal("service exposed a host port")
	}
}

func TestBuildServiceRejectsUnapprovedInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ServiceSpec)
	}{
		{"mutable image", func(s *ServiceSpec) { s.Image = "redis:latest" }},
		{"missing command", func(s *ServiceSpec) { s.Command = nil }},
		{"root UID", func(s *ServiceSpec) { s.UID = 0 }},
		{"bad name", func(s *ServiceSpec) { s.Name = "redis_bad" }},
		{"long name", func(s *ServiceSpec) { s.Name = strings.Repeat("a", 28) }},
		{"invalid port", func(s *ServiceSpec) { s.Port = 0 }},
		{"too much CPU", func(s *ServiceSpec) { s.CPURequest = "1" }},
		{"bad architecture", func(s *ServiceSpec) { s.Architecture = "386" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := validServiceSpec()
			tc.edit(&spec)
			if _, err := BuildServicePod(spec); err == nil {
				t.Fatal("invalid service Pod accepted")
			}
			if _, err := BuildService(spec); err == nil {
				t.Fatal("invalid ClusterIP service accepted")
			}
		})
	}
}
