package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/deployment"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// InstallationPrerequisites shares the exact startup validator and RBAC checks.
// It deliberately does not require the target ConfigMap/Deployment before switch.
func InstallationPrerequisites(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	var problems []error
	problems = append(problems, Preflight(ctx, cfg, client), doctorNamespace(ctx, cfg, client), doctorServiceAccount(ctx, cfg, client), doctorTypeChecking(ctx, client), doctorQuotaStatus(ctx, cfg, client))
	for _, rule := range doctorAccessRules(cfg) {
		if err := doctorAccess(ctx, client, rule); err != nil {
			problems = append(problems, fmt.Errorf("RBAC %s %s/%s: %w", rule.Verb, rule.Namespace, rule.Resource, err))
		}
	}
	return errors.Join(problems...)
}

type Capacity struct {
	Quota         string `json:"quota"`
	Resource      string `json:"resource"`
	Requested     string `json:"requested"`
	Used          string `json:"used"`
	Hard          string `json:"hard"`
	Available     string `json:"available"`
	AfterRecreate string `json:"availableAfterRecreate"`
	Severity      string `json:"severity"`
}

// InstallationCapacity credits only nonterminal Pods owned through ReplicaSets
// of the actual Recreate Deployment. Label matches alone never grant credit.
func InstallationCapacity(ctx context.Context, cfg config.Config, client kubernetes.Interface) ([]Capacity, error) {
	ns := cfg.Kubernetes.LeaseNamespace
	quotas, err := client.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	credit := corev1.ResourceList{}
	d, err := client.AppsV1().Deployments(ns).Get(ctx, "spawnpod", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && d.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType && d.UID != "" {
		rs, err := client.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		owned := map[types.UID]bool{}
		for _, r := range rs.Items {
			for _, owner := range r.OwnerReferences {
				if owner.Kind == "Deployment" && owner.UID == d.UID && owner.Controller != nil && *owner.Controller {
					owned[r.UID] = true
				}
			}
		}
		pods, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range pods.Items {
			p := &pods.Items[i]
			if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue
			}
			for _, owner := range p.OwnerReferences {
				if owner.Kind == "ReplicaSet" && owned[owner.UID] && owner.Controller != nil && *owner.Controller {
					for k, v := range podBudget(p) {
						sum := credit[k]
						sum.Add(v)
						credit[k] = sum
					}
					break
				}
			}
		}
	}
	var result []Capacity
	need := deployment.InstallationBudget()
	for _, quota := range quotas.Items {
		if len(quota.Spec.Scopes) > 0 || quota.Spec.ScopeSelector != nil {
			return result, fmt.Errorf("quota %s has scopes; capacity requires operator verification", quota.Name)
		}
		for _, key := range sortedResourceNames(quota.Spec.Hard) {
			hard := quota.Spec.Hard[key]
			used, ok := quota.Status.Used[key]
			observed, hardOK := quota.Status.Hard[key]
			if !ok || !hardOK || observed.Cmp(hard) != 0 {
				return result, fmt.Errorf("quota %s %s status is not current", quota.Name, key)
			}
			required, charged := need[key]
			if !charged {
				continue
			}
			free := hard.DeepCopy()
			free.Sub(used)
			after := free.DeepCopy()
			released := credit[key]
			if released.Cmp(used) > 0 {
				return result, fmt.Errorf("quota %s %s usage is behind owned Pod resources; wait for quota accounting", quota.Name, key)
			}
			after.Add(released)
			severity := "ok"
			if required.Cmp(after) > 0 {
				severity = "blocker"
			} else if required.Cmp(free) > 0 {
				severity = "warning"
			}
			result = append(result, Capacity{Quota: quota.Name, Resource: string(key), Requested: required.String(), Used: used.String(), Hard: hard.String(), Available: free.String(), AfterRecreate: after.String(), Severity: severity})
		}
	}
	return result, nil
}

func sortedResourceNames(values corev1.ResourceList) []corev1.ResourceName {
	keys := make([]corev1.ResourceName, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// InstallationRunnerSecret checks the required key without returning its value.
func InstallationRunnerSecret(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	return doctorRunnerSecret(ctx, cfg, client)
}
