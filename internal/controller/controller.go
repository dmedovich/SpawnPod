package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/dmedovich/spawnpod/internal/adapter/forgejo"
	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	"github.com/dmedovich/spawnpod/internal/telemetry"
	"github.com/dmedovich/spawnpod/internal/transport/unixrpc"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type reconciler interface{ ReconcileOnce(context.Context) error }

type observedReconciler struct {
	target  reconciler
	metrics *telemetry.State
}

func (r observedReconciler) ReconcileOnce(ctx context.Context) error {
	err := r.target.ReconcileOnce(ctx)
	r.metrics.Reconciled(err)
	return err
}

// Preflight checks the API reads used by recovery and verifies that the Lease
// namespace exists. Lease create/update permissions are tested by election.
func Preflight(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	if client == nil {
		return errors.New("kubernetes client is nil")
	}
	if _, err := client.CoreV1().ConfigMaps(cfg.Kubernetes.Namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list environment ConfigMaps: %w", err)
	}
	if _, err := client.CoreV1().Pods(cfg.Kubernetes.Namespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list job Pods: %w", err)
	}
	if _, err := client.CoordinationV1().Leases(cfg.Kubernetes.LeaseNamespace).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return fmt.Errorf("list controller Leases: %w", err)
	}
	if _, err := client.CoordinationV1().Leases(cfg.Kubernetes.LeaseNamespace).Get(ctx, cfg.Kubernetes.LeaseName, metav1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get controller Lease: %w", err)
	}
	if err := preflightWorkloadAdmission(ctx, cfg, client); err != nil {
		return err
	}
	policy, err := client.NetworkingV1().NetworkPolicies(cfg.Kubernetes.Namespace).Get(ctx, provider.DefaultDenyPolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get spawnPod default-deny NetworkPolicy: %w", err)
	}
	if !validDefaultDeny(policy) {
		return errors.New("spawnPod default-deny NetworkPolicy differs from required policy")
	}
	if cfg.SourceProxy != "" {
		ip, port, err := cfg.SourceProxyEndpoint()
		if err != nil {
			return err
		}
		expected, err := provider.BuildSourceProxyPolicy(ip, port)
		if err != nil {
			return err
		}
		installed, err := client.NetworkingV1().NetworkPolicies(cfg.Kubernetes.Namespace).Get(ctx, provider.SourceProxyPolicyName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get source proxy NetworkPolicy: %w", err)
		}
		if !reflect.DeepEqual(installed.Spec, expected.Spec) {
			return errors.New("source proxy NetworkPolicy differs from required policy")
		}
	}
	return nil
}

func preflightWorkloadAdmission(ctx context.Context, cfg config.Config, client kubernetes.Interface) error {
	expectedPolicy, expectedBinding, err := admission.Objects(cfg)
	if err != nil {
		return fmt.Errorf("build expected workload admission policy: %w", err)
	}
	policy, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get workload admission policy: %w", err)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionv1.Fail || policy.Spec.MatchConstraints == nil || !sameResourceRules(policy.Spec.MatchConstraints.ResourceRules, expectedPolicy.Spec.MatchConstraints.ResourceRules) || len(policy.Spec.MatchConstraints.ExcludeResourceRules) != 0 || !emptyLabelSelector(policy.Spec.MatchConstraints.NamespaceSelector) || !emptyLabelSelector(policy.Spec.MatchConstraints.ObjectSelector) || !defaultMatchPolicy(policy.Spec.MatchConstraints.MatchPolicy) || !reflect.DeepEqual(policy.Spec.Validations, expectedPolicy.Spec.Validations) || policy.Spec.ParamKind != nil || len(policy.Spec.MatchConditions) != 0 {
		return errors.New("installed workload admission policy differs from current profiles")
	}
	binding, err := client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(ctx, admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get workload admission binding: %w", err)
	}
	if binding.Spec.PolicyName != admission.PolicyName || len(binding.Spec.ValidationActions) != 1 || binding.Spec.ValidationActions[0] != admissionv1.Deny || binding.Spec.ParamRef != nil || binding.Spec.MatchResources == nil || binding.Spec.MatchResources.NamespaceSelector == nil || !reflect.DeepEqual(binding.Spec.MatchResources.NamespaceSelector, expectedBinding.Spec.MatchResources.NamespaceSelector) || !emptyLabelSelector(binding.Spec.MatchResources.ObjectSelector) || !defaultMatchPolicy(binding.Spec.MatchResources.MatchPolicy) || len(binding.Spec.MatchResources.ResourceRules) != 0 || len(binding.Spec.MatchResources.ExcludeResourceRules) != 0 {
		return errors.New("installed workload admission binding differs from required scope")
	}
	expectedLimits, err := admission.ContainerLimits(cfg)
	if err != nil {
		return err
	}
	limits, err := client.CoreV1().LimitRanges(cfg.Kubernetes.Namespace).Get(ctx, admission.LimitRangeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get container LimitRange: %w", err)
	}
	if len(limits.Spec.Limits) != 1 || limits.Spec.Limits[0].Type != corev1.LimitTypeContainer || len(limits.Spec.Limits[0].Min) != 0 || len(limits.Spec.Limits[0].MaxLimitRequestRatio) != 0 {
		return errors.New("installed container LimitRange differs from current profiles")
	}
	for _, pair := range [][2]corev1.ResourceList{
		{limits.Spec.Limits[0].Max, expectedLimits.Spec.Limits[0].Max},
		{limits.Spec.Limits[0].Default, expectedLimits.Spec.Limits[0].Default},
		{limits.Spec.Limits[0].DefaultRequest, expectedLimits.Spec.Limits[0].DefaultRequest},
	} {
		if len(pair[0]) != len(pair[1]) {
			return errors.New("installed container LimitRange differs from current profiles")
		}
		for name, expected := range pair[1] {
			actual, ok := pair[0][name]
			if !ok || actual.Cmp(expected) != 0 {
				return errors.New("installed container LimitRange differs from current profiles")
			}
		}
	}
	expectedQuota, err := admission.NamespaceQuota(cfg)
	if err != nil {
		return err
	}
	quota, err := client.CoreV1().ResourceQuotas(cfg.Kubernetes.Namespace).Get(ctx, admission.QuotaName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get namespace ResourceQuota: %w", err)
	}
	if len(quota.Spec.Hard) != len(expectedQuota.Spec.Hard) || len(quota.Spec.Scopes) != 0 || quota.Spec.ScopeSelector != nil {
		return errors.New("installed namespace ResourceQuota differs from current budget")
	}
	for name, expected := range expectedQuota.Spec.Hard {
		actual, ok := quota.Spec.Hard[name]
		if !ok || actual.Cmp(expected) != 0 {
			return errors.New("installed namespace ResourceQuota differs from current budget")
		}
	}
	return nil
}

// The API server fills these admission match defaults on read. Compare their
// meaning so an installed manifest still matches the generated one.
func sameResourceRules(actual, expected []admissionv1.NamedRuleWithOperations) bool {
	if len(actual) != len(expected) {
		return false
	}
	for i := range actual {
		rule := actual[i].DeepCopy()
		if rule.Scope != nil && *rule.Scope == admissionv1.AllScopes {
			rule.Scope = nil
		}
		if !reflect.DeepEqual(*rule, expected[i]) {
			return false
		}
	}
	return true
}

func emptyLabelSelector(selector *metav1.LabelSelector) bool {
	return selector == nil || len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0
}

func defaultMatchPolicy(policy *admissionv1.MatchPolicyType) bool {
	return policy == nil || *policy == admissionv1.Equivalent
}

func validDefaultDeny(policy *networkingv1.NetworkPolicy) bool {
	if policy == nil {
		return false
	}
	selector := policy.Spec.PodSelector
	// Both the namespace-wide baseline and the original managed-Pod baseline
	// cover every job/service Pod. Extra requirements could exclude one.
	if !(len(selector.MatchLabels) == 0 || len(selector.MatchLabels) == 1 && selector.MatchLabels["spawnpod.dev/managed"] == "true") || len(selector.MatchExpressions) != 0 || len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 || len(policy.Spec.PolicyTypes) != 2 {
		return false
	}
	return (policy.Spec.PolicyTypes[0] == networkingv1.PolicyTypeIngress && policy.Spec.PolicyTypes[1] == networkingv1.PolicyTypeEgress) || (policy.Spec.PolicyTypes[0] == networkingv1.PolicyTypeEgress && policy.Spec.PolicyTypes[1] == networkingv1.PolicyTypeIngress)
}

// ReconcileLoop performs an immediate sweep, then repeats until cancellation.
// Each sweep has a bounded context so an unavailable API cannot stall forever.
func ReconcileLoop(ctx context.Context, target reconciler, interval, timeout time.Duration, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if target == nil || interval <= 0 || timeout <= 0 {
		logger.Error("invalid reconciliation loop configuration")
		return
	}
	sweep := func() {
		if ctx.Err() != nil {
			return
		}
		sweepCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := target.ReconcileOnce(sweepCtx); err != nil && ctx.Err() == nil {
			logger.Error("reconciliation failed", "error", err)
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// Run connects the engine to its Kubernetes store and job provider. Only the
// Lease holder may reconcile. Losing leadership stops the process; the caller
// must restart it to rejoin election with a fresh engine instance.
func Run(ctx context.Context, cfg config.Config, client kubernetes.Interface, restConfig *rest.Config, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if client == nil || restConfig == nil {
		return errors.New("kubernetes client and REST config are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cleanupTimeout, _ := cfg.CleanupTimeout()
	drainTimeout, _ := cfg.DrainTimeout()
	interval, _ := cfg.ReconcileInterval()
	preflightCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := Preflight(preflightCtx, cfg, client)
	cancel()
	if err != nil {
		return err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate Lease identity: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}
	identity := hostname + "-" + hex.EncodeToString(nonce[:])
	lock, err := resourcelock.New(resourcelock.LeasesResourceLock, cfg.Kubernetes.LeaseNamespace, cfg.Kubernetes.LeaseName, client.CoreV1(), client.CoordinationV1(), resourcelock.ResourceLockConfig{Identity: identity})
	if err != nil {
		return fmt.Errorf("create Lease lock: %w", err)
	}
	jobs := &provider.Provider{Client: client, Config: restConfig, Namespace: cfg.Kubernetes.Namespace}
	retention, _ := cfg.TombstoneRetention()
	worker := &engine.Engine{Store: &provider.ConfigMapStore{Client: client, Namespace: cfg.Kubernetes.Namespace}, Jobs: &provider.EngineJobs{Provider: jobs}, Services: &provider.EngineServices{Provider: jobs}, CleanupTimeout: cleanupTimeout, TombstoneRetention: retention, Logger: logger}
	plugin, err := forgejo.New(worker, worker.Store, cfg)
	if err != nil {
		return err
	}
	logger.Info("waiting for controller Lease", "lease", cfg.Kubernetes.LeaseNamespace+"/"+cfg.Kubernetes.LeaseName, "identity", identity)
	// Keep renewing the Lease until operator-initiated drain completes. A
	// genuine Lease loss still cancels leaderCtx and stops RPCs immediately.
	electionCtx, stopElection := context.WithCancel(context.WithoutCancel(ctx))
	defer stopElection()
	metrics := &telemetry.State{}
	metrics.API(nil)
	observed := observedReconciler{target: worker, metrics: metrics}
	started := make(chan struct{})
	serviceErrors := make(chan error, 1)
	serviceDone := make(chan struct{})
	reportFailure := func(err error) {
		select {
		case serviceErrors <- err:
		default:
		}
		stopElection()
	}
	if cfg.Controller.MonitorAddress != "" {
		listener, err := net.Listen("tcp", cfg.Controller.MonitorAddress)
		if err != nil {
			return fmt.Errorf("listen monitor: %w", err)
		}
		monitor := &http.Server{Handler: metrics.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
		defer func() { _ = monitor.Close() }()
		go func() {
			if err := monitor.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				reportFailure(fmt.Errorf("monitor: %w", err))
			}
		}()
	}
	go func() {
		select {
		case <-ctx.Done():
			metrics.Drain()
			select {
			case <-started: /* Serve observes ctx and drains. */
			default:
				stopElection()
			}
		case <-electionCtx.Done():
		}
	}()
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock: lock, LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second,
		// Do not release early: ongoing cleanup must finish or time out before
		// another process can acquire the Lease after its expiry.
		ReleaseOnCancel: false,
		Callbacks: leaderelection.LeaderCallbacks{OnStartedLeading: func(leaderCtx context.Context) {
			close(started)
			defer close(serviceDone)
			defer stopElection()
			logger.Info("controller Lease acquired")
			initialCtx, cancel := context.WithTimeout(leaderCtx, cleanupTimeout)
			initialErr := observed.ReconcileOnce(initialCtx)
			cancel()
			if initialErr != nil {
				if leaderCtx.Err() == nil {
					reportFailure(fmt.Errorf("initial reconciliation: %w", initialErr))
				}
				return
			}
			go ReconcileLoop(leaderCtx, observed, interval, cleanupTimeout, logger)
			go monitorAPI(leaderCtx, cfg, client, metrics)
			logger.Info("serving Forgejo plugin", "socket", cfg.Controller.SocketPath)
			if err := unixrpc.ServeWithOptions(leaderCtx, cfg.Controller.SocketPath, plugin, unixrpc.Options{Shutdown: ctx, DrainTimeout: drainTimeout, Metrics: metrics}); err != nil && leaderCtx.Err() == nil {
				reportFailure(fmt.Errorf("forgejo RPC server: %w", err))
			}
		}, OnStoppedLeading: func() { metrics.Serving(false); logger.Info("controller Lease released or lost") }},
	})
	if err != nil {
		return fmt.Errorf("configure leader election: %w", err)
	}
	elector.Run(electionCtx)
	select {
	case <-started:
		select {
		case <-serviceDone:
		case <-time.After(5 * time.Second):
			logger.Error("Forgejo service did not stop promptly")
		}
	default:
	}
	if ctx.Err() != nil {
		return nil
	}
	select {
	case err := <-serviceErrors:
		return err
	default:
	}
	return errors.New("controller Lease lost")
}

func monitorAPI(ctx context.Context, cfg config.Config, client kubernetes.Interface, metrics *telemetry.State) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := client.CoreV1().ConfigMaps(cfg.Kubernetes.Namespace).List(probeCtx, metav1.ListOptions{Limit: 1})
		cancel()
		metrics.API(err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
