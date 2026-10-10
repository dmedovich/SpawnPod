package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	yaml "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type Config struct {
	Version     int                `yaml:"version"`
	Kubernetes  Kubernetes         `yaml:"kubernetes"`
	Controller  Controller         `yaml:"controller"`
	AgentImage  string             `yaml:"agentImage"`
	SourceProxy string             `yaml:"sourceProxy"`
	Profiles    map[string]Profile `yaml:"profiles"`
}

type Kubernetes struct {
	Namespace      string `yaml:"namespace"`
	Kubeconfig     string `yaml:"kubeconfig"`
	LeaseNamespace string `yaml:"leaseNamespace"`
	LeaseName      string `yaml:"leaseName"`
	Quota          Quota  `yaml:"quota"`
}

// Quota is an operator-selected namespace budget, not a node capacity probe.
// Object counts include all objects in the dedicated workload namespace.
type Quota struct {
	MaxPods            int64  `yaml:"maxPods"`
	MaxConfigMaps      int64  `yaml:"maxConfigMaps"`
	MaxServices        int64  `yaml:"maxServices"`
	MaxNetworkPolicies int64  `yaml:"maxNetworkPolicies"`
	RequestsCPU        string `yaml:"requestsCPU"`
	LimitsCPU          string `yaml:"limitsCPU"`
	RequestsMemory     string `yaml:"requestsMemory"`
	LimitsMemory       string `yaml:"limitsMemory"`
	RequestsStorage    string `yaml:"requestsStorage"`
	LimitsStorage      string `yaml:"limitsStorage"`
}

type Controller struct {
	ReconcileInterval string `yaml:"reconcileInterval"`
	CleanupTimeout    string `yaml:"cleanupTimeout"`
	SocketPath        string `yaml:"socketPath"`
	EnableServices    bool   `yaml:"enableServices"`
	DrainTimeout      string `yaml:"drainTimeout"`
	MonitorAddress    string `yaml:"monitorAddress"`
}

type Profile struct {
	Network        string                    `yaml:"network"`
	Image          string                    `yaml:"image"`
	Architecture   string                    `yaml:"architecture"`
	UID            int64                     `yaml:"uid"`
	GID            int64                     `yaml:"gid"`
	CPURequest     string                    `yaml:"cpuRequest"`
	CPULimit       string                    `yaml:"cpuLimit"`
	MemoryRequest  string                    `yaml:"memoryRequest"`
	MemoryLimit    string                    `yaml:"memoryLimit"`
	StorageRequest string                    `yaml:"storageRequest"`
	StorageLimit   string                    `yaml:"storageLimit"`
	WorkspaceSize  string                    `yaml:"workspaceSize"`
	MaxLifetime    string                    `yaml:"maxLifetime"`
	Services       map[string]ServiceProfile `yaml:"services"`
}

// ServiceProfile approves one service image and its fixed runtime bounds.
// Workflow environment variables are not part of this configuration.
type ServiceProfile struct {
	Image          string   `yaml:"image"`
	Command        []string `yaml:"command"`
	Architecture   string   `yaml:"architecture"`
	UID            int64    `yaml:"uid"`
	GID            int64    `yaml:"gid"`
	Port           int32    `yaml:"port"`
	CPURequest     string   `yaml:"cpuRequest"`
	CPULimit       string   `yaml:"cpuLimit"`
	MemoryRequest  string   `yaml:"memoryRequest"`
	MemoryLimit    string   `yaml:"memoryLimit"`
	StorageRequest string   `yaml:"storageRequest"`
	StorageLimit   string   `yaml:"storageLimit"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer func() { _ = file.Close() }()
	return Decode(file)
}

// Decode rejects unknown YAML fields and trailing documents. It does not
// interpolate environment variables or expand shell expressions.
func Decode(reader io.Reader) (Config, error) {
	decoder := yaml.NewDecoder(reader)
	decoder.KnownFields(true)
	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("multiple YAML documents are not supported")
		}
		return Config{}, fmt.Errorf("trailing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Version != 1 {
		return errors.New("unsupported config version")
	}
	if !validLabel(c.Kubernetes.Namespace) || !validLabel(c.Kubernetes.LeaseNamespace) || !validLabel(c.Kubernetes.LeaseName) {
		return errors.New("invalid Kubernetes namespace or Lease name")
	}
	if _, err := c.ReconcileInterval(); err != nil {
		return err
	}
	if _, err := c.CleanupTimeout(); err != nil {
		return err
	}
	if _, err := c.DrainTimeout(); err != nil {
		return err
	}
	if address := c.Controller.MonitorAddress; address != "" {
		host, port, err := net.SplitHostPort(address)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || n < 1 || n > 65535 || (host != "" && net.ParseIP(host) == nil) {
			return errors.New("monitorAddress must contain an IP (or empty host) and a valid port")
		}
	}
	if !filepath.IsAbs(c.Controller.SocketPath) || filepath.Clean(c.Controller.SocketPath) != c.Controller.SocketPath || len(c.Controller.SocketPath) > 100 || strings.ContainsRune(c.Controller.SocketPath, 0) {
		return errors.New("invalid controller socketPath")
	}
	if len(c.Profiles) == 0 {
		return errors.New("at least one execution profile is required")
	}
	sourceEnabled := false
	for _, profile := range c.Profiles {
		if profile.Network == "source" {
			sourceEnabled = true
		} else if profile.Network != "" && profile.Network != "isolated" {
			return errors.New("network must be isolated or source")
		}
	}
	if sourceEnabled {
		if _, _, err := c.SourceProxyEndpoint(); err != nil {
			return err
		}
	} else if c.SourceProxy != "" {
		return errors.New("sourceProxy requires a source profile")
	}
	if err := c.Kubernetes.Quota.Validate(c.Profiles); err != nil {
		return fmt.Errorf("kubernetes quota: %w", err)
	}
	for name, profile := range c.Profiles {
		if !validLabel(name) {
			return fmt.Errorf("invalid profile name %q", name)
		}
		if _, err := profile.Lifetime(); err != nil {
			return fmt.Errorf("profile %s: %w", name, err)
		}
		spec := profile.JobSpec(c.AgentImage)
		spec.EnvironmentID = strings.Repeat("a", 32)
		_, err := provider.BuildJobPod(spec)
		if err != nil {
			return fmt.Errorf("profile %s: %w", name, err)
		}
		for serviceName, serviceProfile := range profile.Services {
			serviceSpec := serviceProfile.Spec(serviceName, c.AgentImage)
			serviceSpec.EnvironmentID = strings.Repeat("a", 32)
			if serviceSpec.Architecture != profile.Architecture {
				return fmt.Errorf("profile %s service %s: architecture differs from job", name, serviceName)
			}
			if _, err := provider.BuildServicePod(serviceSpec); err != nil {
				return fmt.Errorf("profile %s service %s: %w", name, serviceName, err)
			}
			if _, err := provider.BuildService(serviceSpec); err != nil {
				return fmt.Errorf("profile %s service %s: %w", name, serviceName, err)
			}
		}
	}
	return nil
}

// Hard returns the exact ResourceQuota keys installed for the workload
// namespace. Call only after Validate succeeds.
func (q Quota) Hard() corev1.ResourceList {
	return corev1.ResourceList{
		"count/pods":       resource.MustParse(strconv.FormatInt(q.MaxPods, 10)),
		"count/configmaps": resource.MustParse(strconv.FormatInt(q.MaxConfigMaps, 10)),
		"count/services":   resource.MustParse(strconv.FormatInt(q.MaxServices, 10)),
		"count/networkpolicies.networking.k8s.io": resource.MustParse(strconv.FormatInt(q.MaxNetworkPolicies, 10)),
		corev1.ResourceRequestsCPU:                resource.MustParse(q.RequestsCPU),
		corev1.ResourceLimitsCPU:                  resource.MustParse(q.LimitsCPU),
		corev1.ResourceRequestsMemory:             resource.MustParse(q.RequestsMemory),
		corev1.ResourceLimitsMemory:               resource.MustParse(q.LimitsMemory),
		corev1.ResourceRequestsEphemeralStorage:   resource.MustParse(q.RequestsStorage),
		corev1.ResourceLimitsEphemeralStorage:     resource.MustParse(q.LimitsStorage),
	}
}

func (q Quota) Validate(profiles map[string]Profile) error {
	sourcePolicy := int64(0)
	for _, profile := range profiles {
		if profile.Network == "source" {
			sourcePolicy = 1
		}
	}
	for name, value := range map[string]int64{"maxPods": q.MaxPods, "maxConfigMaps": q.MaxConfigMaps, "maxServices": q.MaxServices, "maxNetworkPolicies": q.MaxNetworkPolicies} {
		if value < 1 || value > 10000 {
			return fmt.Errorf("%s must be between 1 and 10000", name)
		}
	}
	for _, pair := range [][2]string{{q.RequestsCPU, q.LimitsCPU}, {q.RequestsMemory, q.LimitsMemory}, {q.RequestsStorage, q.LimitsStorage}} {
		request, requestErr := resource.ParseQuantity(pair[0])
		limit, limitErr := resource.ParseQuantity(pair[1])
		if requestErr != nil || limitErr != nil || request.Sign() <= 0 || limit.Sign() <= 0 || request.Cmp(limit) > 0 {
			return errors.New("invalid aggregate resource requests or limits")
		}
	}
	if q.MaxConfigMaps < 1 || q.MaxNetworkPolicies < 1 {
		return errors.New("quota must hold one environment record and default-deny policy")
	}
	for name, profile := range profiles {
		services := int64(len(profile.Services))
		if q.MaxPods < services+1 || q.MaxServices < services || q.MaxNetworkPolicies < 1+sourcePolicy+2*services {
			return fmt.Errorf("profile %s resource count exceeds quota", name)
		}
		for _, resourcePair := range []struct {
			request, limit           string
			quotaRequest, quotaLimit string
			serviceValues            func(ServiceProfile) (string, string)
		}{
			{profile.CPURequest, profile.CPULimit, q.RequestsCPU, q.LimitsCPU, func(s ServiceProfile) (string, string) { return s.CPURequest, s.CPULimit }},
			{profile.MemoryRequest, profile.MemoryLimit, q.RequestsMemory, q.LimitsMemory, func(s ServiceProfile) (string, string) { return s.MemoryRequest, s.MemoryLimit }},
			{profile.StorageRequest, profile.StorageLimit, q.RequestsStorage, q.LimitsStorage, func(s ServiceProfile) (string, string) { return s.StorageRequest, s.StorageLimit }},
		} {
			request, err := resource.ParseQuantity(resourcePair.request)
			if err != nil {
				return fmt.Errorf("profile %s has invalid resource request", name)
			}
			limit, err := resource.ParseQuantity(resourcePair.limit)
			if err != nil {
				return fmt.Errorf("profile %s has invalid resource limit", name)
			}
			for _, service := range profile.Services {
				serviceRequest, serviceLimit := resourcePair.serviceValues(service)
				parsedRequest, requestErr := resource.ParseQuantity(serviceRequest)
				parsedLimit, limitErr := resource.ParseQuantity(serviceLimit)
				if requestErr != nil || limitErr != nil {
					return fmt.Errorf("profile %s has invalid service resources", name)
				}
				request.Add(parsedRequest)
				limit.Add(parsedLimit)
			}
			if request.Cmp(resource.MustParse(resourcePair.quotaRequest)) > 0 || limit.Cmp(resource.MustParse(resourcePair.quotaLimit)) > 0 {
				return fmt.Errorf("profile %s cannot fit one complete environment within quota", name)
			}
		}
	}
	return nil
}

func validLabel(value string) bool { return len(value) <= 63 && dnsLabel.MatchString(value) }

func parseBoundedDuration(value, name string, min, max time.Duration) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < min || parsed > max {
		return 0, fmt.Errorf("invalid %s: must be between %s and %s", name, min, max)
	}
	return parsed, nil
}

func (c Config) ReconcileInterval() (time.Duration, error) {
	return parseBoundedDuration(c.Controller.ReconcileInterval, "reconcileInterval", time.Second, 10*time.Minute)
}
func (c Config) CleanupTimeout() (time.Duration, error) {
	return parseBoundedDuration(c.Controller.CleanupTimeout, "cleanupTimeout", time.Second, 10*time.Minute)
}
func (c Config) DrainTimeout() (time.Duration, error) {
	if c.Controller.DrainTimeout == "" {
		return 30 * time.Second, nil
	}
	return parseBoundedDuration(c.Controller.DrainTimeout, "drainTimeout", time.Second, 5*time.Minute)
}
func (p Profile) Lifetime() (time.Duration, error) {
	return parseBoundedDuration(p.MaxLifetime, "maxLifetime", time.Minute, 24*time.Hour)
}

// JobSpec returns approved Pod input. The engine assigns EnvironmentID later.
func (p Profile) JobSpec(agentImage string) engine.JobSpec {
	return engine.JobSpec{Network: p.Network, Image: p.Image, Architecture: p.Architecture, AgentImage: agentImage, UID: p.UID, GID: p.GID, CPURequest: p.CPURequest, CPULimit: p.CPULimit, MemoryRequest: p.MemoryRequest, MemoryLimit: p.MemoryLimit, StorageRequest: p.StorageRequest, StorageLimit: p.StorageLimit, WorkspaceSize: p.WorkspaceSize}
}

// SourceProxyEndpoint returns the admin-pinned IP and port admitted by the
// source NetworkPolicy. DNS names are intentionally unsupported here.
func (c Config) SourceProxyEndpoint() (netip.Addr, int32, error) {
	parsed, err := url.Parse(c.SourceProxy)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return netip.Addr{}, 0, errors.New("sourceProxy must be an HTTP URL with an IP and port")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return netip.Addr{}, 0, errors.New("sourceProxy must include an IP and port")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsValid() || !ip.IsGlobalUnicast() || ip.Zone() != "" {
		return netip.Addr{}, 0, errors.New("sourceProxy must use a unicast IP")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return netip.Addr{}, 0, errors.New("sourceProxy has invalid port")
	}
	return ip, int32(number), nil
}

// Spec converts an approved service profile to a renderable Kubernetes input.
// The engine assigns EnvironmentID during Create.
func (p ServiceProfile) Spec(name, agentImage string) provider.ServiceSpec {
	return provider.ServiceSpec{Name: name, Image: p.Image, AgentImage: agentImage, Command: p.Command, Architecture: p.Architecture, UID: p.UID, GID: p.GID, Port: p.Port, CPURequest: p.CPURequest, CPULimit: p.CPULimit, MemoryRequest: p.MemoryRequest, MemoryLimit: p.MemoryLimit, StorageRequest: p.StorageRequest, StorageLimit: p.StorageLimit}
}
