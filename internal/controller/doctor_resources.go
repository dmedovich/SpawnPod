package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/config"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func profileBudget(cfg config.Config, profile config.Profile) corev1.ResourceList {
	spec := profile.JobSpec(cfg.AgentImage)
	spec.EnvironmentID = strings.Repeat("a", 32)
	pod, _ := provider.BuildJobPod(spec) // cfg.Validate has already checked it.
	budget := podBudget(pod)
	budget["count/configmaps"] = resource.MustParse("1")
	for name, service := range profile.Services {
		spec := service.Spec(name, cfg.AgentImage)
		spec.EnvironmentID = strings.Repeat("a", 32)
		pod, _ := provider.BuildServicePod(spec)
		for key, value := range podBudget(pod) {
			sum := budget[key]
			sum.Add(value)
			budget[key] = sum
		}
	}
	count := resource.NewQuantity(int64(len(profile.Services)), resource.DecimalSI)
	budget["count/services"] = *count
	budget["services"] = count.DeepCopy()
	budget["count/networkpolicies.networking.k8s.io"] = *resource.NewQuantity(int64(2*len(profile.Services)), resource.DecimalSI)
	return budget
}

// Generated Pods have ordinary init containers and no restartable sidecars.
// Kubernetes charges the maximum of summed app resources and each init phase.
func podBudget(pod *corev1.Pod) corev1.ResourceList {
	budget := corev1.ResourceList{"count/pods": resource.MustParse("1"), "pods": resource.MustParse("1")}
	for _, prefix := range []string{"requests.", "limits."} {
		values := func(c corev1.Container) corev1.ResourceList {
			if prefix == "requests." {
				return c.Resources.Requests
			}
			return c.Resources.Limits
		}
		total := corev1.ResourceList{}
		for _, c := range pod.Spec.Containers {
			for key, value := range values(c) {
				sum := total[key]
				sum.Add(value)
				total[key] = sum
			}
		}
		for _, c := range pod.Spec.InitContainers {
			for key, value := range values(c) {
				if sum := total[key]; value.Cmp(sum) > 0 {
					total[key] = value.DeepCopy()
				}
			}
		}
		for key, value := range total {
			budget[corev1.ResourceName(prefix+string(key))] = value
		}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourceEphemeralStorage} {
		if value, ok := budget[corev1.ResourceName("requests."+string(name))]; ok {
			budget[name] = value.DeepCopy()
		}
	}
	return budget
}

func quotaWarnings(quota *corev1.ResourceQuota, need corev1.ResourceList) []string {
	if len(quota.Spec.Scopes) != 0 || quota.Spec.ScopeSelector != nil {
		return []string{fmt.Sprintf("quota %s has scopes; verify placement against those scopes manually", quota.Name)}
	}
	var warnings []string
	keys := make([]string, 0, len(quota.Spec.Hard))
	for key := range quota.Spec.Hard {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := corev1.ResourceName(key)
		hard := quota.Spec.Hard[name]
		used, observed := quota.Status.Used[name]
		if !observed {
			warnings = append(warnings, fmt.Sprintf("quota %s %s usage not observed; wait for quota controller before rollout", quota.Name, key))
			continue
		}
		free := hard.DeepCopy()
		free.Sub(used)
		required := need[name]
		if required.Cmp(free) > 0 {
			hint := "wait for active workloads to finish or review namespace budget"
			if key == "count/configmaps" {
				hint = "review cleaned record retention or namespace ConfigMap budget"
			}
			warnings = append(warnings, fmt.Sprintf("quota %s %s: available=%s required=%s; %s", quota.Name, key, free.String(), required.String(), hint))
		}
	}
	return warnings
}

func doctorRunnerSecret(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	secret, err := client.CoreV1().Secrets(cfg.Kubernetes.LeaseNamespace).Get(ctx, "spawnpod-runner", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(secret.Data["runner.yaml"]) == 0 {
		return errors.New("spawnpod-runner requires a nonempty runner.yaml key mounted at /etc/runner/runner.yaml; create it from the private Runner configuration file")
	}
	return nil
}

func doctorMountedConfig(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	cm, err := client.CoreV1().ConfigMaps(cfg.Kubernetes.LeaseNamespace).Get(ctx, "spawnpod-config", metav1.GetOptions{})
	if err != nil {
		return err
	}
	mounted, err := config.Decode(strings.NewReader(cm.Data["config.yaml"]))
	if err != nil {
		return errors.New("spawnpod-config requires valid config.yaml; regenerate with render-deployment")
	}
	if mounted.Kubernetes.Kubeconfig != "" || mounted.Controller.SocketPath != "/run/spawnpod/private/backend.sock" || mounted.Controller.MonitorAddress != ":9090" {
		return errors.New("mounted config runtime paths differ from installation contract")
	}
	expected, err := config.InstallationBytes(cfg)
	if err != nil {
		return errors.New("cannot normalize operator config")
	}
	actual, err := config.InstallationBytes(mounted)
	if err != nil {
		return errors.New("cannot normalize mounted config")
	}
	if !bytes.Equal(actual, expected) {
		return errors.Join(fmt.Errorf("mounted config.yaml differs from operator config (expected sha256:%x; actual sha256:%x); regenerate the installation bundle", sha256.Sum256(expected), sha256.Sum256(actual)), Preflight(ctx, mounted, client))
	}
	return Preflight(ctx, mounted, client)
}
