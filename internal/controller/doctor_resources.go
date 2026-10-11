package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/config"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	yaml "go.yaml.in/yaml/v3"
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
			warnings = append(warnings, fmt.Sprintf("quota %s %s: available=%s required=%s; wait for cleanup, shorten controller.tombstoneRetention for cleaned records, or review namespace budget before rollout", quota.Name, key, free.String(), required.String()))
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
	// These values are deliberately normalized by the installation renderer.
	cfg.Kubernetes.Kubeconfig = ""
	cfg.Controller.SocketPath = "/run/spawnpod/private/backend.sock"
	cfg.Controller.MonitorAddress = ":9090"
	// Canonical YAML treats absent and empty maps alike after a render/decode.
	expected, err := yaml.Marshal(cfg)
	if err != nil {
		return errors.New("cannot serialize operator config")
	}
	actual, err := yaml.Marshal(mounted)
	if err != nil {
		return errors.New("cannot serialize mounted config")
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("mounted config.yaml differs from operator config; regenerate with render-deployment")
	}
	return nil
}
