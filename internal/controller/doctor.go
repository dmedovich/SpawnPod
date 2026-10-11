package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/deployment"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Doctor checks the installed security baseline using the controller's own
// credentials. It does not create workloads or prove runtime policy enforcement.
func Doctor(ctx context.Context, cfg config.Config, client kubernetes.Interface, out io.Writer) error {
	return DoctorWithOptions(ctx, cfg, client, out, DoctorOptions{})
}

type DoctorOptions struct {
	Strict bool
	// Installation requires operator access to infra Secrets/ConfigMaps/quotas.
	// Controller credentials deliberately lack access to Runner credentials.
	Installation bool
}

func DoctorWithOptions(ctx context.Context, cfg config.Config, client kubernetes.Interface, out io.Writer, options DoctorOptions) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}
	if client == nil || out == nil {
		return errors.New("kubernetes client and output writer are required")
	}
	var reportErr error
	write := func(format string, args ...any) {
		if reportErr == nil {
			_, reportErr = fmt.Fprintf(out, format, args...)
		}
	}
	var failures []string
	passed, warnings := 0, 0
	check := func(name string, err error) {
		if err == nil {
			passed++
			write("OK   %s\n", name)
			return
		}
		write("FAIL %s: %v\n", name, err)
		failures = append(failures, name)
	}
	warn := func(message string) {
		warnings++
		write("WARN %s\n", message)
		if options.Strict {
			failures = append(failures, message)
		}
	}
	check("controller preflight", Preflight(ctx, cfg, client))
	check("workload namespace Pod Security", doctorNamespace(ctx, cfg, client))
	check("tokenless job ServiceAccount", doctorServiceAccount(ctx, cfg, client))
	check("admission expression type checking", doctorTypeChecking(ctx, client))
	check("namespace quota status", doctorQuotaStatus(ctx, cfg, client))
	quota, quotaErr := client.CoreV1().ResourceQuotas(cfg.Kubernetes.Namespace).Get(ctx, admission.QuotaName, metav1.GetOptions{})
	if quotaErr == nil {
		names := make([]string, 0, len(cfg.Profiles))
		for name := range cfg.Profiles {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, message := range quotaWarnings(quota, profileBudget(cfg, cfg.Profiles[name])) {
				warn("profile " + name + ": " + message)
			}
		}
	}
	images := []string{cfg.AgentImage}
	for _, profile := range cfg.Profiles {
		images = append(images, profile.Image)
		for _, service := range profile.Services {
			images = append(images, service.Image)
		}
	}
	for _, image := range images {
		host := strings.SplitN(image, "/", 2)[0]
		if host == "localhost" || strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "[::1]") {
			warn("node-local image reference: import the pinned image on every eligible node or publish it to a reachable registry")
			break
		}
	}
	if options.Installation {
		check("Runner Secret runner.yaml (contents withheld)", doctorRunnerSecret(ctx, cfg, client))
		check("mounted controller config.yaml", doctorMountedConfig(ctx, cfg, client))
		quotas, err := client.CoreV1().ResourceQuotas(cfg.Kubernetes.LeaseNamespace).List(ctx, metav1.ListOptions{})
		check("installation namespace quotas", err)
		if err == nil {
			for i := range quotas.Items {
				for _, message := range quotaWarnings(&quotas.Items[i], deployment.InstallationBudget()) {
					warn("installation: " + message)
				}
			}
		}
	}
	for _, rule := range doctorAccessRules(cfg) {
		name := fmt.Sprintf("RBAC %s %s/%s %s", rule.Verb, rule.Namespace, rule.Resource, rule.Name)
		check(name, doctorAccess(ctx, client, rule))
	}
	write("Doctor is read-only; use separate live scenarios to verify admission denials, NetworkPolicy isolation, scheduling, image pulls and Runner execution.\n")
	write("Result: %d passed, %d warnings, %d failures (strict=%t).\n", passed, warnings, len(failures), options.Strict)
	if len(failures) != 0 {
		return errors.Join(reportErr, fmt.Errorf("doctor: %d check(s) failed: %s", len(failures), strings.Join(failures, ", ")))
	}
	return reportErr
}

func doctorNamespace(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	ns, err := client.CoreV1().Namespaces().Get(ctx, cfg.Kubernetes.Namespace, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, mode := range []string{"enforce", "audit", "warn"} {
		if ns.Labels["pod-security.kubernetes.io/"+mode] != "restricted" || ns.Labels["pod-security.kubernetes.io/"+mode+"-version"] != "v1.34" {
			return fmt.Errorf("%s must be restricted at v1.34", mode)
		}
	}
	if ns.Labels["spawnpod.dev/role"] != "workload" {
		return errors.New("spawnpod.dev/role must be workload")
	}
	return nil
}

func doctorServiceAccount(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	sa, err := client.CoreV1().ServiceAccounts(cfg.Kubernetes.Namespace).Get(ctx, "spawnpod-job", metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		return errors.New("spawnpod-job must set automountServiceAccountToken: false")
	}
	return nil
}

func doctorTypeChecking(ctx context.Context, client kubernetes.Interface) error {
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if policy.Generation == 0 || policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
		return errors.New("admission controller has not type-checked the current policy generation")
	}
	if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) > 0 {
		return fmt.Errorf("%d expression warning(s): %s: %s", len(warnings), warnings[0].FieldRef, warnings[0].Warning)
	}
	return nil
}

func doctorQuotaStatus(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	quota, err := client.CoreV1().ResourceQuotas(cfg.Kubernetes.Namespace).Get(ctx, admission.QuotaName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(quota.Status.Hard) != len(quota.Spec.Hard) {
		return errors.New("quota controller has not observed the current hard limits")
	}
	for name, hard := range quota.Spec.Hard {
		observed, ok := quota.Status.Hard[name]
		if !ok || observed.Cmp(hard) != 0 {
			return fmt.Errorf("quota controller has not observed %s", name)
		}
	}
	return nil
}

func doctorAccess(ctx context.Context, client kubernetes.Interface, rule authorizationv1.ResourceAttributes) error {
	review, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &rule},
	}, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	if !review.Status.Allowed {
		if review.Status.EvaluationError != "" {
			return errors.New(review.Status.EvaluationError)
		}
		return fmt.Errorf("denied: %s", review.Status.Reason)
	}
	return nil
}

func doctorAccessRules(cfg config.Config) []authorizationv1.ResourceAttributes {
	var rules []authorizationv1.ResourceAttributes
	add := func(namespace, group, resource, name string, verbs ...string) {
		for _, verb := range verbs {
			rules = append(rules, authorizationv1.ResourceAttributes{Namespace: namespace, Group: group, Resource: resource, Name: name, Verb: verb})
		}
	}
	ns := cfg.Kubernetes.Namespace
	add(ns, "", "configmaps", "", "get", "list", "create", "update", "delete")
	add(ns, "", "pods", "", "get", "list", "create", "delete")
	add(ns, "", "pods/exec", "", "create")
	add(ns, "", "services", "", "get", "create", "delete")
	add(ns, "networking.k8s.io", "networkpolicies", "", "get", "create", "delete")
	add(ns, "", "limitranges", admission.LimitRangeName, "get")
	add(ns, "", "resourcequotas", admission.QuotaName, "get")
	add(ns, "", "serviceaccounts", "spawnpod-job", "get")
	add(cfg.Kubernetes.LeaseNamespace, "coordination.k8s.io", "leases", "", "list", "create")
	add(cfg.Kubernetes.LeaseNamespace, "coordination.k8s.io", "leases", cfg.Kubernetes.LeaseName, "get", "update")
	add("", "", "namespaces", ns, "get")
	add("", "admissionregistration.k8s.io", "validatingadmissionpolicies", admission.PolicyName, "get")
	add("", "admissionregistration.k8s.io", "validatingadmissionpolicybindings", admission.PolicyName, "get")
	return rules
}
