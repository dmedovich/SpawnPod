package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/controller"
	"github.com/dmedovich/spawnpod/internal/installation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func runUpgrade(args []string) error {
	if len(args) == 0 || (args[0] != "plan" && args[0] != "render" && args[0] != "check") {
		return errors.New("usage: spawnpod upgrade <plan|render|check> --config PATH --image RUNTIME@sha256:DIGEST --runner-image RUNNER@sha256:DIGEST [--target VERSION]; apply and automatic rollback are not implemented")
	}
	command := args[0]
	flags := flag.NewFlagSet("upgrade "+command, flag.ContinueOnError)
	path := flags.String("config", "", "source configuration (never modified)")
	target := flags.String("target", version, "release matching this CLI binary; image identity is pinned separately")
	image := flags.String("image", "", "digest-pinned runtime image shared by Controller and Agent; overrides source agentImage in generated bundle")
	runner := flags.String("runner-image", "", "digest-pinned Forgejo Runner image; existing credentials are preserved")
	var output, manifests, kubeconfig, phase string
	var checkOnly bool
	switch command {
	case "render":
		flags.StringVar(&output, "output-dir", "", "directory for managed bundle files")
		flags.BoolVar(&checkOnly, "check", false, "report changes without writing; exit nonzero if changes are needed")
	case "check":
		flags.StringVar(&manifests, "manifests", "", "bundle directory to verify before live reads")
		flags.StringVar(&kubeconfig, "kubeconfig", "", "operator kubeconfig (overrides config path)")
		flags.StringVar(&phase, "phase", "ready", "switch: drained and stopped, target dependencies; ready: target installation health")
	case "plan":
		flags.StringVar(&kubeconfig, "kubeconfig", "", "optional operator kubeconfig for read-only live discovery; without it plan is offline")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *image == "" || *runner == "" {
		return errors.New("--config, --image and --runner-image are required")
	}
	if version != "dev" && *target != version {
		return errors.New("target must match this CLI version; use the target release binary to render its bundle")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	bundle, err := installation.Build(cfg, *target, *image, *runner)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if command == "render" {
		if output == "" {
			return errors.New("--output-dir is required")
		}
		changes, err := bundle.RenderFiles(output, checkOnly)
		if err != nil {
			return err
		}
		if changes == nil {
			changes = []string{}
		}
		if err := encoder.Encode(struct {
			Revision  string   `json:"revision"`
			Changes   []string `json:"changes"`
			CheckOnly bool     `json:"checkOnly"`
		}{bundle.Index.Revision, changes, checkOnly}); err != nil {
			return err
		}
		if checkOnly && len(changes) > 0 {
			return errors.New("bundle changes required (no files written)")
		}
		return nil
	}
	if command == "plan" {
		plan := struct {
			Bundle            installation.Index `json:"bundle"`
			Transformations   []string           `json:"transformations"`
			Offline           bool               `json:"offline"`
			CurrentAgent      string             `json:"currentAgent,omitempty"`
			CurrentController string             `json:"currentController,omitempty"`
			AgentChanges      bool               `json:"agentChanges"`
			ActivePods        int                `json:"managedPods,omitempty"`
			Warning           string             `json:"warning"`
		}{Bundle: bundle.Index, Transformations: []string{"agentImage := --image (same as Controller)", "kubeconfig := empty (in-cluster credentials)", "socketPath := /run/spawnpod/private/backend.sock", "monitorAddress := :9090", "resolve tombstoneRetention and network defaults"}, Offline: kubeconfig == "", Warning: "Plan is not permission to skip gates. Validate image platform support/pull, keep a verified previous full bundle, and use separate GitOps commits/sync gates. COMPLETE requires a real Forgejo smoke job."}
		if kubeconfig != "" {
			cfg.Kubernetes.Kubeconfig = kubeconfig
			client, _, err := configureClient(cfg, false)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			mounted, err := installation.MountedConfig(ctx, client, cfg.Kubernetes.LeaseNamespace)
			if err != nil {
				return err
			}
			if mounted.Kubernetes.Namespace != cfg.Kubernetes.Namespace || mounted.Kubernetes.LeaseNamespace != cfg.Kubernetes.LeaseNamespace || mounted.Kubernetes.LeaseName != cfg.Kubernetes.LeaseName {
				return errors.New("source and mounted namespace/Lease identities differ; automatic namespace or Lease migration is not supported")
			}
			controllerClient, _, err := configureClient(cfg, true)
			if err != nil {
				return err
			}
			if err := controller.InstallationPrerequisites(ctx, mounted, controllerClient); err != nil {
				return fmt.Errorf("current installation prerequisite failed: %w", err)
			}
			plan.CurrentAgent = mounted.AgentImage
			plan.AgentChanges = mounted.AgentImage != *image
			dep, err := client.AppsV1().Deployments(cfg.Kubernetes.LeaseNamespace).Get(ctx, "spawnpod", metav1.GetOptions{})
			if err != nil {
				return err
			}
			for _, c := range dep.Spec.Template.Spec.Containers {
				if c.Name == "controller" {
					plan.CurrentController = c.Image
				}
			}
			pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "spawnpod.dev/managed=true"})
			if err != nil {
				return err
			}
			plan.ActivePods = len(pods.Items)
		}
		return encoder.Encode(plan)
	}
	if manifests == "" {
		return errors.New("--manifests is required")
	}
	if err := bundle.VerifyFiles(manifests); err != nil {
		return err
	}
	if kubeconfig != "" {
		cfg.Kubernetes.Kubeconfig = kubeconfig
	}
	client, _, err := configureClient(cfg, false)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	controllerClient, _, err := configureClient(cfg, true)
	if err != nil {
		return err
	}
	report := bundle.Check(ctx, client, controllerClient, phase)
	return errors.Join(encoder.Encode(report), report.Err())
}
