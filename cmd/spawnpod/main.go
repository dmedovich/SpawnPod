// spawnpod is the controller and local Forgejo plugin entry point.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/controller"
	"github.com/dmedovich/spawnpod/internal/deployment"
	"github.com/dmedovich/spawnpod/internal/smoke"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if len(args) > 0 && args[0] == "environments" {
		return runEnvironments(args[1:])
	}
	if len(args) == 0 || (args[0] != "serve" && args[0] != "validate" && args[0] != "render-admission" && args[0] != "render-deployment" && args[0] != "doctor" && args[0] != "smoke") {
		return errors.New("usage: spawnpod <serve|validate|render-admission|render-deployment|doctor|smoke> --config PATH; spawnpod environments <list|inspect|cleanup>; or spawnpod version")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	path := flags.String("config", "", "path to strict YAML configuration")
	var controllerImage, runnerImage *string
	if args[0] == "render-deployment" {
		controllerImage = flags.String("controller-image", "", "digest-pinned SpawnPod runtime image")
		runnerImage = flags.String("runner-image", "", "digest-pinned official Forgejo Runner v13.1.0 image")
	}
	asController := flags.Bool("as-controller", false, "impersonate the controller ServiceAccount with an admin kubeconfig")
	strict := flags.Bool("strict", false, "doctor: treat warnings as failures")
	installation := flags.Bool("installation", false, "doctor: also check Runner Secret, mounted config and infra quota using operator credentials")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 || (*asController && args[0] != "doctor" && args[0] != "serve") {
		return errors.New("--config PATH is required; --as-controller is available only for doctor and serve")
	}
	if (*strict || *installation) && args[0] != "doctor" || *installation && *asController {
		return errors.New("--strict and --installation require doctor; --installation uses operator credentials, without --as-controller")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if args[0] == "validate" {
		fmt.Println("configuration valid")
		return nil
	}
	if args[0] == "render-deployment" {
		manifest, err := deployment.Render(cfg, *controllerImage, *runnerImage)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(manifest)
		return err
	}
	if args[0] == "render-admission" {
		manifest, err := admission.Render(cfg)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(manifest)
		return err
	}
	if args[0] == "smoke" {
		profile, ok := cfg.Profiles["default"]
		if !ok {
			return errors.New("smoke requires a default profile")
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		smokeCtx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		defer cancel()
		return smoke.Run(smokeCtx, cfg.Controller.SocketPath, profile.Image, "default", os.Stdout)
	}
	client, restConfig, err := configureClient(cfg, *asController)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if args[0] == "doctor" {
		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return controller.DoctorWithOptions(checkCtx, cfg, client, os.Stdout, controller.DoctorOptions{Strict: *strict, Installation: *installation})
	}
	return controller.Run(ctx, cfg, client, restConfig, slog.Default())
}

func configureClient(cfg config.Config, asController bool) (kubernetes.Interface, *rest.Config, error) {
	if asController && cfg.Kubernetes.Kubeconfig == "" {
		return nil, nil, errors.New("--as-controller requires an explicit admin kubeconfig")
	}
	var restConfig *rest.Config
	var err error
	if cfg.Kubernetes.Kubeconfig == "" {
		restConfig, err = rest.InClusterConfig()
	} else {
		restConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubernetes.Kubeconfig)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("configure Kubernetes client: %w", err)
	}
	if asController {
		namespace := cfg.Kubernetes.LeaseNamespace
		restConfig.Impersonate.UserName = "system:serviceaccount:" + namespace + ":spawnpod-controller"
		restConfig.Impersonate.Groups = []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"}
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return client, restConfig, nil
}

func runEnvironments(args []string) error {
	if len(args) == 0 || (args[0] != "list" && args[0] != "inspect" && args[0] != "cleanup") {
		return errors.New("usage: spawnpod environments list --config PATH; inspect --config PATH ID; cleanup --config PATH --dry-run")
	}
	command := args[0]
	flags := flag.NewFlagSet("environments "+command, flag.ContinueOnError)
	path := flags.String("config", "", "path to strict YAML configuration")
	dryRun := flags.Bool("dry-run", false, "preview cleanup without modifying resources")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *path == "" || (command == "inspect" && flags.NArg() != 1) || (command != "inspect" && flags.NArg() != 0) || (command == "cleanup" && !*dryRun) || (command != "cleanup" && *dryRun) {
		return errors.New("usage: spawnpod environments list --config PATH; inspect --config PATH ID; cleanup --config PATH --dry-run")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	client, _, err := configureClient(cfg, false)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	retention, _ := cfg.TombstoneRetention()
	switch command {
	case "list":
		return controller.ListEnvironments(checkCtx, client, cfg.Kubernetes.Namespace, os.Stdout)
	case "inspect":
		return controller.InspectEnvironment(checkCtx, client, cfg.Kubernetes.Namespace, flags.Arg(0), os.Stdout, retention)
	default:
		return controller.PreviewCleanup(checkCtx, client, cfg.Kubernetes.Namespace, time.Now(), os.Stdout, retention)
	}
}
