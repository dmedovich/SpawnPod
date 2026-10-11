package config

import (
	"bytes"

	yaml "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// InstallationConfig is the explicit source-to-mounted configuration transform.
// The YAML round trip also separates profile maps from the caller's config.
func InstallationConfig(source Config) (Config, error) {
	if err := source.Validate(); err != nil {
		return Config{}, err
	}
	b, err := yaml.Marshal(source)
	if err != nil {
		return Config{}, err
	}
	c, err := Decode(bytes.NewReader(b))
	if err != nil {
		return Config{}, err
	}
	c.Kubernetes.Kubeconfig = ""
	c.Controller.SocketPath = "/run/spawnpod/private/backend.sock"
	c.Controller.MonitorAddress = ":9090"
	interval, _ := c.ReconcileInterval()
	c.Controller.ReconcileInterval = interval.String()
	cleanup, _ := c.CleanupTimeout()
	c.Controller.CleanupTimeout = cleanup.String()
	drain, _ := c.DrainTimeout()
	c.Controller.DrainTimeout = drain.String()
	normalizeResources(&c.Kubernetes.Quota.RequestsCPU, &c.Kubernetes.Quota.LimitsCPU, &c.Kubernetes.Quota.RequestsMemory, &c.Kubernetes.Quota.LimitsMemory, &c.Kubernetes.Quota.RequestsStorage, &c.Kubernetes.Quota.LimitsStorage)
	retention, _ := c.TombstoneRetention()
	c.Controller.TombstoneRetention = retention.String()
	for name, profile := range c.Profiles {
		if profile.Network == "" {
			profile.Network = "isolated"
		}
		lifetime, _ := profile.Lifetime()
		profile.MaxLifetime = lifetime.String()
		normalizeResources(&profile.CPURequest, &profile.CPULimit, &profile.MemoryRequest, &profile.MemoryLimit, &profile.StorageRequest, &profile.StorageLimit, &profile.WorkspaceSize)
		for name, service := range profile.Services {
			normalizeResources(&service.CPURequest, &service.CPULimit, &service.MemoryRequest, &service.MemoryLimit, &service.StorageRequest, &service.StorageLimit)
			profile.Services[name] = service
		}
		c.Profiles[name] = profile
	}
	return c, c.Validate()
}

// InstallationBytes compares effective semantics, including documented defaults.
func InstallationBytes(source Config) ([]byte, error) {
	c, err := InstallationConfig(source)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(c)
}

func normalizeResources(values ...*string) {
	for i, value := range values {
		quantity := resource.MustParse(*value)
		if i < 2 {
			quantity.Format = resource.DecimalSI
		} else {
			quantity.Format = resource.BinarySI
		}
		*value = resource.NewDecimalQuantity(*quantity.AsDec(), quantity.Format).String()
	}
}
