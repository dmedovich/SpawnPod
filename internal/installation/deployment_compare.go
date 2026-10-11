package installation

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// normalizeDeploymentDefaults enumerates API defaults observed in the reference
// topology. Unexpected Pod fields remain visible, including host namespace use,
// added capabilities, extra mounts/env and changed credential projections.
func normalizeDeploymentDefaults(spec map[string]any) {
	setDefault(spec, "progressDeadlineSeconds", float64(600))
	template, _ := spec["template"].(map[string]any)
	pod, _ := template["spec"].(map[string]any)
	if pod == nil {
		return
	}
	for k, v := range map[string]any{"dnsPolicy": "ClusterFirst", "restartPolicy": "Always", "schedulerName": "default-scheduler", "serviceAccount": pod["serviceAccountName"]} {
		setDefault(pod, k, v)
	}
	containers, _ := pod["containers"].([]any)
	for _, value := range containers {
		c, _ := value.(map[string]any)
		if c == nil {
			continue
		}
		for k, v := range map[string]any{"imagePullPolicy": "IfNotPresent", "terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File"} {
			setDefault(c, k, v)
		}
		ports, _ := c["ports"].([]any)
		for _, value := range ports {
			if port, ok := value.(map[string]any); ok {
				setDefault(port, "protocol", "TCP")
			}
		}
		for _, key := range []string{"livenessProbe", "readinessProbe", "startupProbe"} {
			if probe, ok := c[key].(map[string]any); ok {
				setDefault(probe, "successThreshold", float64(1))
				if http, ok := probe["httpGet"].(map[string]any); ok {
					setDefault(http, "scheme", "HTTP")
				}
			}
		}
	}
	volumes, _ := pod["volumes"].([]any)
	for _, value := range volumes {
		v, _ := value.(map[string]any)
		for _, key := range []string{"configMap", "secret", "projected"} {
			if source, ok := v[key].(map[string]any); ok {
				setDefault(source, "defaultMode", float64(420))
			}
		}
	}
}
func setDefault(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}
func compareDeployment(actual, expected any) error {
	a, _ := json.Marshal(actual)
	e, _ := json.Marshal(expected)
	var am, em map[string]any
	_ = json.Unmarshal(a, &am)
	_ = json.Unmarshal(e, &em)
	normalizeDeploymentDefaults(am)
	normalizeDeploymentDefaults(em)
	if !reflect.DeepEqual(am, em) {
		field := deploymentDifference("deployment.spec", am, em)
		return fmt.Errorf("%s differs from bundle (values withheld)", field)
	}
	return nil
}
func deploymentDifference(path string, a, e any) string {
	// Preserve deterministic first-field diagnostics with full (not subset) comparison.
	if am, ok := a.(map[string]any); ok {
		if em, ok := e.(map[string]any); ok {
			union := map[string]any{}
			for k, v := range am {
				union[k] = v
			}
			for k, v := range em {
				if _, ok := union[k]; !ok {
					union[k] = v
				}
			}
			if d := subsetDifference(path, am, union); d != "" {
				return d
			}
			if d := subsetDifference(path, em, union); d != "" {
				return d
			}
		}
	}
	return path
}
