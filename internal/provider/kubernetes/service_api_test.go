package kubernetes

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestServiceResourceLifecycleAndUIDCleanup(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	provider := &Provider{Client: client, Namespace: "ci"}
	spec := validServiceSpec()
	handle, err := provider.CreateServiceResources(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	service, err := client.CoreV1().Services("ci").Get(ctx, handle.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service.UID = types.UID("service-uid")
	service.Spec.ClusterIP = "10.43.1.9"
	if _, err := client.CoreV1().Services("ci").Update(ctx, service, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := client.CoreV1().Pods("ci").Get(ctx, handle.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.UID = types.UID("pod-uid")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := client.CoreV1().Pods("ci").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	egressName, ingressName := servicePolicyNames(spec.EnvironmentID, spec.Name)
	for name, uid := range map[string]types.UID{egressName: "egress-uid", ingressName: "ingress-uid"} {
		policy, err := client.NetworkingV1().NetworkPolicies("ci").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		policy.UID = uid
		if _, err := client.NetworkingV1().NetworkPolicies("ci").Update(ctx, policy, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	handle, err = provider.FindServiceResources(ctx, spec.EnvironmentID, spec.Name)
	if err != nil || handle.PodUID != "pod-uid" || handle.ServiceUID != "service-uid" || handle.EgressPolicyUID != "egress-uid" || handle.IngressPolicyUID != "ingress-uid" {
		t.Fatalf("recovered handle=%+v err=%v", handle, err)
	}
	address, err := provider.ResolveServiceAddress(ctx, spec, handle)
	if err != nil || address != "10.43.1.9" {
		t.Fatalf("resolved address=%q err=%v", address, err)
	}
	ip, err := provider.WaitServiceReady(ctx, spec, handle)
	if err != nil || ip != "10.43.1.9" {
		t.Fatalf("ready ip=%q err=%v", ip, err)
	}
	if alive, err := provider.ServiceAlive(ctx, spec, handle); err != nil || !alive {
		t.Fatalf("ready service reported dead: alive=%v err=%v", alive, err)
	}
	pod.Status.Phase = corev1.PodFailed
	if _, err := client.CoreV1().Pods("ci").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if alive, err := provider.ServiceAlive(ctx, spec, handle); err != nil || alive {
		t.Fatalf("failed service reported alive: alive=%v err=%v", alive, err)
	}
	wrong := handle
	wrong.PodUID = "other-pod"
	if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, wrong); err == nil {
		t.Fatal("cleanup accepted changed recorded Pod UID")
	}
	if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, handle); err != nil {
		t.Fatal(err)
	}
	if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, handle); err != nil {
		t.Fatal("cleanup was not repeatable:", err)
	}
	var podDelete, serviceDelete, egressDelete, ingressDelete bool
	for _, action := range client.Actions() {
		opts, ok := action.(interface{ GetDeleteOptions() metav1.DeleteOptions })
		if !ok || action.GetVerb() != "delete" {
			continue
		}
		preconditions := opts.GetDeleteOptions().Preconditions
		if preconditions == nil || preconditions.UID == nil {
			t.Fatal("deletion omitted UID precondition")
		}
		switch action.GetResource().Resource {
		case "pods":
			podDelete = *preconditions.UID == "pod-uid"
		case "services":
			serviceDelete = *preconditions.UID == "service-uid"
		case "networkpolicies":
			if action.(interface{ GetName() string }).GetName() == egressName {
				egressDelete = *preconditions.UID == "egress-uid"
			} else if action.(interface{ GetName() string }).GetName() == ingressName {
				ingressDelete = *preconditions.UID == "ingress-uid"
			}
		}
	}
	if !podDelete || !serviceDelete || !egressDelete || !ingressDelete {
		t.Fatal("service resources and policies were not deleted with their UIDs")
	}
}

func TestServiceCreateRecoversPartialAPIState(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("Pod admission rejected")
	})
	provider := &Provider{Client: client, Namespace: "ci"}
	spec := validServiceSpec()
	if _, err := provider.CreateServiceResources(ctx, spec); err == nil {
		t.Fatal("partial create did not report Pod failure")
	}
	service, err := client.CoreV1().Services("ci").Get(ctx, serviceResourceName(spec), metav1.GetOptions{})
	if err != nil {
		t.Fatal("Service was not created before Pod:", err)
	}
	service.UID = "service-uid"
	if _, err := client.CoreV1().Services("ci").Update(ctx, service, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	egressName, ingressName := servicePolicyNames(spec.EnvironmentID, spec.Name)
	for name, uid := range map[string]types.UID{egressName: "egress-uid", ingressName: "ingress-uid"} {
		policy, err := client.NetworkingV1().NetworkPolicies("ci").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		policy.UID = uid
		if _, err := client.NetworkingV1().NetworkPolicies("ci").Update(ctx, policy, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := provider.FindServiceResources(ctx, spec.EnvironmentID, spec.Name)
	if err != nil || handle.PodUID != "" || handle.ServiceUID != "service-uid" || handle.EgressPolicyUID != "egress-uid" || handle.IngressPolicyUID != "ingress-uid" {
		t.Fatalf("partial handle=%+v err=%v", handle, err)
	}
	if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, handle); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{egressName, ingressName} {
		if _, err := client.NetworkingV1().NetworkPolicies("ci").Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("partial policy %s remains: %v", name, err)
		}
	}
}

func TestServiceCreateRecoversLostSuccessResponse(t *testing.T) {
	for _, step := range []string{"egress", "ingress", "service", "pod"} {
		t.Run(step, func(t *testing.T) {
			ctx := context.Background()
			client := fake.NewSimpleClientset()
			provider := &Provider{Client: client, Namespace: "ci"}
			spec := validServiceSpec()
			egressName, ingressName := servicePolicyNames(spec.EnvironmentID, spec.Name)
			target := map[string]string{
				"egress": egressName, "ingress": ingressName,
				"service": serviceResourceName(spec), "pod": serviceResourceName(spec),
			}[step]
			resource := map[string]string{
				"egress": "networkpolicies", "ingress": "networkpolicies",
				"service": "services", "pod": "pods",
			}[step]
			client.PrependReactor("create", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
				created := action.(ktesting.CreateAction).GetObject().DeepCopyObject()
				object := created.(metav1.Object)
				lost := action.GetResource().Resource == resource && object.GetName() == target
				if lost {
					object.SetUID("lost-response-uid")
				} else {
					object.SetUID(types.UID("uid-" + object.GetName()))
				}
				if err := client.Tracker().Create(action.GetResource(), created, action.GetNamespace()); err != nil {
					return true, nil, err
				}
				if lost {
					return true, nil, errors.New("API response lost after create")
				}
				return true, created, nil
			})
			if _, err := provider.CreateServiceResources(ctx, spec); err == nil {
				t.Fatal("lost response did not report an error")
			}
			recovered, err := provider.FindServiceResources(ctx, spec.EnvironmentID, spec.Name)
			if err != nil {
				t.Fatal(err)
			}
			var hiddenUID types.UID
			switch step {
			case "egress":
				hiddenUID = recovered.EgressPolicyUID
			case "ingress":
				hiddenUID = recovered.IngressPolicyUID
			case "service":
				hiddenUID = recovered.ServiceUID
			case "pod":
				hiddenUID = recovered.PodUID
			}
			if hiddenUID != "lost-response-uid" {
				t.Fatalf("created resource was not recovered: %+v", recovered)
			}
			if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, recovered); err != nil {
				t.Fatal(err)
			}
			remaining, err := provider.FindServiceResources(ctx, spec.EnvironmentID, spec.Name)
			if err != nil || remaining.PodUID != "" || remaining.ServiceUID != "" || remaining.EgressPolicyUID != "" || remaining.IngressPolicyUID != "" {
				t.Fatalf("resources remain after recovery: %+v, %v", remaining, err)
			}
		})
	}
}

func TestServiceRecoveryRejectsForeignPolicy(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	provider := &Provider{Client: client, Namespace: "ci"}
	spec := validServiceSpec()
	egress, _, err := BuildServicePolicies(spec)
	if err != nil {
		t.Fatal(err)
	}
	egress.Labels[serviceLabel] = "other"
	egress.UID = "foreign-uid"
	if _, err := client.NetworkingV1().NetworkPolicies("ci").Create(ctx, egress, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.FindServiceResources(ctx, spec.EnvironmentID, spec.Name); err == nil {
		t.Fatal("foreign policy was adopted")
	}
	if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, ServiceHandle{}); err == nil {
		t.Fatal("foreign policy was deleted")
	}
}

func TestServiceRecoveryRejectsForeignResource(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	provider := &Provider{Client: client, Namespace: "ci"}
	spec := validServiceSpec()
	foreign, err := BuildService(spec)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Labels[serviceLabel] = "other"
	foreign.UID = "foreign-uid"
	if _, err := client.CoreV1().Services("ci").Create(ctx, foreign, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.FindServiceResources(ctx, spec.EnvironmentID, spec.Name); err == nil {
		t.Fatal("foreign resource was adopted")
	}
	if err := provider.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, ServiceHandle{}); err == nil {
		t.Fatal("foreign resource was removed")
	}
}
