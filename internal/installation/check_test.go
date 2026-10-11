package installation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	ktesting "k8s.io/client-go/testing"
)

func installed(t *testing.T, b *Bundle) *kubefake.Clientset {
	t.Helper()
	objects := []runtime.Object{}
	var deployment *appsv1.Deployment
	for _, name := range manifestNames() {
		all, err := DecodeObjects(b.Files[name])
		if err != nil {
			t.Fatal(err)
		}
		for _, obj := range all {
			data, _ := json.Marshal(obj.Object)
			typed, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			switch o := typed.(type) {
			case *appsv1.Deployment:
				o.Generation = 1
				o.Status = appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
				deployment = o
			case *admissionv1.ValidatingAdmissionPolicy:
				o.Generation = 1
				o.Status.ObservedGeneration = 1
				o.Status.TypeChecking = &admissionv1.TypeChecking{}
			case *corev1.ResourceQuota:
				o.Status.Hard = o.Spec.Hard.DeepCopy()
				o.Status.Used = corev1.ResourceList{}
				for k := range o.Spec.Hard {
					o.Status.Used[k] = resource.MustParse("0")
				}
			}
			objects = append(objects, typed)
		}
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "installation", Namespace: "infra", Labels: deployment.Spec.Template.Labels}, Spec: deployment.Spec.Template.Spec, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}, {Name: "runner", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "local"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{Architecture: "amd64"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	for _, image := range b.Index.Images {
		node.Status.Images = append(node.Status.Images, corev1.ContainerImage{Names: []string{image.Reference}})
	}
	objects = append(objects, pod, node, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-runner", Namespace: "infra"}, Data: map[string][]byte{"runner.yaml": []byte("PRIVATE_TOKEN")}})
	c := kubefake.NewClientset(objects...)
	c.Discovery().(*fake.FakeDiscovery).FakedServerVersion = &kversion.Info{GitVersion: "v1.34.0+k3s1"}
	c.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
		review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).DeepCopy()
		review.Status.Allowed = true
		return true, review, nil
	})
	return c
}
func TestCheckGatesReadOnlyAndNoCIClaim(t *testing.T) {
	b := example(t)
	c := installed(t, b)
	ctx := context.Background()
	report := b.Check(ctx, c, c, "ready")
	if err := report.Err(); err != nil {
		t.Fatalf("%v %+v", err, report.Findings)
	}
	if report.CIProven {
		t.Fatal("readiness claimed CI success")
	}
	for _, a := range c.Actions() {
		if a.GetVerb() != "get" && a.GetVerb() != "list" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			t.Fatalf("mutating action: %+v", a)
		}
	}
	data, _ := json.Marshal(report)
	if strings.Contains(string(data), "PRIVATE_TOKEN") {
		t.Fatal("Secret leaked")
	}
	if report := b.Check(ctx, c, c, "switch"); report.Err() == nil {
		t.Fatal("switch allowed with live installation")
	}
	dep, _ := c.AppsV1().Deployments("infra").Get(ctx, "spawnpod", metav1.GetOptions{})
	zero := int32(0)
	dep.Spec.Replicas = &zero
	dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 1}
	_, _ = c.AppsV1().Deployments("infra").Update(ctx, dep, metav1.UpdateOptions{})
	_ = c.CoreV1().Pods("infra").Delete(ctx, "installation", metav1.DeleteOptions{})
	if r := b.Check(ctx, c, c, "switch"); r.Err() != nil {
		t.Fatalf("drained gate failed: %+v", r.Findings)
	}
	_, _ = c.CoreV1().Pods("ci").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "active", Labels: map[string]string{"spawnpod.dev/managed": "true"}}}, metav1.CreateOptions{})
	if r := b.Check(ctx, c, c, "switch"); r.Err() == nil {
		t.Fatal("old active workload allowed across policy contraction")
	}
}
func TestCheckCrashLoopLocalImageAndRBACDrift(t *testing.T) {
	b := example(t)
	ctx := context.Background()
	t.Run("crash", func(t *testing.T) {
		c := installed(t, b)
		p, _ := c.CoreV1().Pods("infra").Get(ctx, "installation", metav1.GetOptions{})
		p.Status.ContainerStatuses[0].Ready = false
		p.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		_, _ = c.CoreV1().Pods("infra").Update(ctx, p, metav1.UpdateOptions{})
		if r := b.Check(ctx, c, c, "ready"); r.Err() == nil {
			t.Fatal("CrashLoop accepted")
		}
	})
	t.Run("rbac", func(t *testing.T) {
		c := installed(t, b)
		role, _ := c.RbacV1().Roles("ci").Get(ctx, "spawnpod-workloads", metav1.GetOptions{})
		role.Rules[0].Verbs = append(role.Rules[0].Verbs, "*")
		_, _ = c.RbacV1().Roles("ci").Update(ctx, role, metav1.UpdateOptions{})
		if r := b.Check(ctx, c, c, "ready"); r.Err() == nil {
			t.Fatal("extra RBAC permission accepted")
		}
	})
	t.Run("local image", func(t *testing.T) {
		source := b.Config
		image := "localhost/spawnpod@sha256:" + strings.Repeat("a", 64)
		b, err := Build(source, b.Index.Target, image, b.Index.Images[1].Reference)
		if err != nil {
			t.Fatal(err)
		}
		c := installed(t, b)
		n, _ := c.CoreV1().Nodes().Get(ctx, "local", metav1.GetOptions{})
		n.Status.Images = nil
		_, _ = c.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		if r := b.Check(ctx, c, c, "ready"); r.Err() == nil {
			t.Fatal("unverified node-local image accepted")
		}
	})
	t.Run("controller identity", func(t *testing.T) {
		c := installed(t, b)
		restricted := installed(t, b)
		restricted.PrependReactor("create", "selfsubjectaccessreviews", func(action ktesting.Action) (bool, runtime.Object, error) {
			review := action.(ktesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview).DeepCopy()
			return true, review, nil
		})
		if r := b.Check(ctx, c, restricted, "ready"); r.Err() == nil {
			t.Fatal("operator rights masked controller denial")
		}
	})
}

func TestUnexpectedDeploymentSecurityFieldsBlockReadiness(t *testing.T) {
	b := example(t)
	ctx := context.Background()
	for _, mutate := range []func(*appsv1.Deployment){
		func(d *appsv1.Deployment) { d.Spec.Template.Spec.HostNetwork = true },
		func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
		},
		func(d *appsv1.Deployment) {
			d.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "EXTRA", Value: "secret-value"}}
		},
	} {
		c := installed(t, b)
		d, _ := c.AppsV1().Deployments("infra").Get(ctx, "spawnpod", metav1.GetOptions{})
		mutate(d)
		_, _ = c.AppsV1().Deployments("infra").Update(ctx, d, metav1.UpdateOptions{})
		r := b.Check(ctx, c, c, "ready")
		if r.Err() == nil {
			t.Fatal("unexpected installation security field accepted")
		}
		data, _ := json.Marshal(r)
		if strings.Contains(string(data), "secret-value") {
			t.Fatal("deployment diagnostics leaked values")
		}
	}
}
