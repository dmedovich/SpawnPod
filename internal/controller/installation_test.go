package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRecreateCapacityCreditsOnlyOwnedPods(t *testing.T) {
	cfg := exampleControllerConfig(t)
	yes := true
	owner := func(kind string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, UID: uid, Controller: &yes}}
	}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "infra-quota", Namespace: "infra"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("4500m")}}, Status: corev1.ResourceQuotaStatus{Hard: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("4500m")}, Used: corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("4100m")}}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "spawnpod", Namespace: "infra", UID: "deployment"}, Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "infra", UID: "rs", OwnerReferences: owner("Deployment", "deployment")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "infra", OwnerReferences: owner("ReplicaSet", "rs")}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "controller", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}}}}}}
	client := fake.NewClientset(quota, dep, rs, pod)
	checks, err := InstallationCapacity(context.Background(), cfg, client)
	if err != nil || len(checks) != 1 || checks[0].Severity != "warning" || checks[0].AfterRecreate != "2400m" {
		t.Fatalf("Recreate misclassified: %+v %v", checks, err)
	}
	pod.OwnerReferences = nil
	_, _ = client.CoreV1().Pods("infra").Update(context.Background(), pod, metav1.UpdateOptions{})
	checks, err = InstallationCapacity(context.Background(), cfg, client)
	if err != nil || checks[0].Severity != "blocker" {
		t.Fatalf("credited foreign Pod: %+v %v", checks, err)
	}
	pod.OwnerReferences = owner("ReplicaSet", "rs")
	_, _ = client.CoreV1().Pods("infra").Update(context.Background(), pod, metav1.UpdateOptions{})
	dep.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	_, _ = client.AppsV1().Deployments("infra").Update(context.Background(), dep, metav1.UpdateOptions{})
	checks, err = InstallationCapacity(context.Background(), cfg, client)
	if err != nil || checks[0].Severity != "blocker" {
		t.Fatalf("credited RollingUpdate Pod: %+v %v", checks, err)
	}
}

func TestInstallationCapacityChecksCPUQuotaAlias(t *testing.T) {
	cfg := exampleControllerConfig(t)
	hard := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "cpu-alias", Namespace: "infra"}, Spec: corev1.ResourceQuotaSpec{Hard: hard}, Status: corev1.ResourceQuotaStatus{Hard: hard.DeepCopy(), Used: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")}}}
	c := fake.NewClientset(quota)
	capacity, err := InstallationCapacity(context.Background(), cfg, c)
	if err != nil || len(capacity) != 1 || capacity[0].Severity != "blocker" {
		t.Fatalf("CPU quota alias ignored: %+v %v", capacity, err)
	}
}
