package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/engine"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func validJobSpec() JobSpec {
	return JobSpec{
		EnvironmentID: strings.Repeat("a", 32),
		Image:         "registry.example/job@sha256:" + strings.Repeat("b", 64),
		Architecture:  "amd64",
		AgentImage:    "registry.example/agent@sha256:" + strings.Repeat("c", 64),
		UID:           10001, GID: 10001,
		CPURequest: "100m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "512Mi",
		StorageRequest: "1Gi", StorageLimit: "2Gi", WorkspaceSize: "512Mi",
	}
}

func TestBuildJobPodSecurityAndWorkspace(t *testing.T) {
	spec := validJobSpec()
	spec.ServiceAliases = []engine.ServiceAlias{{Name: "redis", IP: "10.43.1.9"}, {Name: "postgres", IP: "10.43.1.10"}}
	pod, err := BuildJobPod(spec)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("job Pod may restart")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("service account token mounted")
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 1 {
		t.Fatal("unexpected container topology")
	}
	job := pod.Spec.Containers[0]
	if job.SecurityContext == nil || job.SecurityContext.RunAsNonRoot == nil || !*job.SecurityContext.RunAsNonRoot {
		t.Fatal("job is not restricted to non-root")
	}
	if job.SecurityContext.AllowPrivilegeEscalation == nil || *job.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation allowed")
	}
	if job.SecurityContext.Capabilities == nil || len(job.SecurityContext.Capabilities.Drop) != 1 || job.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("capabilities are not dropped")
	}
	if job.SecurityContext.SeccompProfile == nil || job.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("seccomp missing")
	}
	if job.Command[0] != agentPath || job.Command[1] != "serve" {
		t.Fatal("job does not launch supervisor")
	}
	if pod.Spec.NodeSelector["kubernetes.io/arch"] != "amd64" {
		t.Fatal("Pod architecture is not pinned")
	}
	if len(pod.Spec.HostAliases) != 2 || pod.Spec.HostAliases[0].Hostnames[0] != "postgres" || pod.Spec.HostAliases[0].IP != "10.43.1.10" || pod.Spec.HostAliases[1].Hostnames[0] != "redis" {
		t.Fatalf("service aliases not rendered deterministically: %+v", pod.Spec.HostAliases)
	}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir == nil {
			t.Fatal("unexpected volume type")
		}
	}
	foundTools := false
	for _, mount := range job.VolumeMounts {
		if mount.Name == "tools" && mount.ReadOnly {
			foundTools = true
		}
	}
	if !foundTools {
		t.Fatal("agent tools volume is writable in job")
	}
}

func TestBuildJobPodRejectsUnsafeInput(t *testing.T) {
	cases := []struct {
		name   string
		change func(*JobSpec)
	}{
		{"mutable image", func(s *JobSpec) { s.Image = "registry.example/job:latest" }},
		{"root user", func(s *JobSpec) { s.UID = 0 }},
		{"oversized workspace", func(s *JobSpec) { s.WorkspaceSize = "3Gi" }},
		{"invalid ID", func(s *JobSpec) { s.EnvironmentID = "job" }},
		{"CPU over limit", func(s *JobSpec) { s.CPURequest = "2" }},
		{"foreign service address", func(s *JobSpec) { s.ServiceAliases = []engine.ServiceAlias{{Name: "redis", IP: "127.0.0.1"}} }},
		{"reserved service name", func(s *JobSpec) { s.ServiceAliases = []engine.ServiceAlias{{Name: "localhost", IP: "10.43.1.9"}} }},
		{"duplicate service name", func(s *JobSpec) {
			s.ServiceAliases = []engine.ServiceAlias{{Name: "redis", IP: "10.43.1.9"}, {Name: "redis", IP: "10.43.1.10"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validJobSpec()
			tc.change(&s)
			if _, err := BuildJobPod(s); err == nil {
				t.Fatal("unsafe spec accepted")
			}
		})
	}
}

func TestCreateWaitAndUIDDelete(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	provider := &Provider{Client: client, Namespace: "ci"}
	created, err := provider.CreateJob(ctx, validJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	uid := types.UID("pod-uid-1")
	pod, err := client.CoreV1().Pods("ci").Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.UID = uid
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := client.CoreV1().Pods("ci").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.WaitReady(ctx, created.Name, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.WaitReady(ctx, created.Name, types.UID("wrong")); err == nil {
		t.Fatal("wrong UID accepted")
	}
	if err := provider.RemoveJob(ctx, created.Name, uid); err != nil {
		t.Fatal(err)
	}
	actions := client.Actions()
	var options metav1.DeleteOptions
	foundDelete := false
	for _, action := range actions {
		if action.GetVerb() != "delete" {
			continue
		}
		deleteAction, ok := action.(interface{ GetDeleteOptions() metav1.DeleteOptions })
		if !ok {
			t.Fatal("delete action did not expose options")
		}
		options = deleteAction.GetDeleteOptions()
		foundDelete = true
	}
	if !foundDelete {
		t.Fatal("delete action did not expose options")
	}
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != uid {
		t.Fatal("delete omitted UID precondition")
	}
}

func TestRemoveJobRetriesAfterLostDeleteResponse(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	provider := &Provider{Client: client, Namespace: "ci"}
	created, err := provider.CreateJob(ctx, validJobSpec())
	if err != nil {
		t.Fatal(err)
	}
	pod, err := client.CoreV1().Pods("ci").Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.UID = "job-uid"
	if _, err := client.CoreV1().Pods("ci").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		if err := client.Tracker().Delete(action.GetResource(), action.GetNamespace(), action.(ktesting.DeleteAction).GetName()); err != nil {
			return true, nil, err
		}
		return true, nil, errors.New("API response lost after Pod deletion")
	})
	if err := provider.RemoveJob(ctx, created.Name, pod.UID); err == nil {
		t.Fatal("lost Delete response was not reported")
	}
	if _, err := client.CoreV1().Pods("ci").Get(ctx, created.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Pod was not deleted before response loss: %v", err)
	}
	if err := provider.RemoveJob(ctx, created.Name, pod.UID); err != nil {
		t.Fatalf("retry did not accept already deleted Pod: %v", err)
	}
}
