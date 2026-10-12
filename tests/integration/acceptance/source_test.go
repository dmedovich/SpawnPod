package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/installation"
	"github.com/dmedovich/spawnpod/internal/sourceproxy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This gate uses the actual official Runner and controller/agent containers.
// Network fixtures alone do not establish Runner profile selection or Exec env.
func (c *cluster) sourceRunnerProfiles(t *testing.T) {
	runnerImage := os.Getenv("SPAWNPOD_ACCEPTANCE_RUNNER_IMAGE")
	runnerPath := os.Getenv("SPAWNPOD_ACCEPTANCE_RUNNER_CONFIG")
	tokenPath := os.Getenv("SPAWNPOD_ACCEPTANCE_FORGEJO_TOKEN")
	host, address := os.Getenv("SPAWNPOD_SOURCE_UPSTREAM_HOST"), os.Getenv("SPAWNPOD_SOURCE_UPSTREAM_IP")
	if c.cfg.SourceProxy == "" {
		t.Skip("source profile is not configured")
	}
	if runnerImage == "" || runnerPath == "" || tokenPath == "" || host == "" || address == "" || c.cfg.SourceProxyPolicy != "pod-selector" {
		t.Fatal("source Runner gate requires pinned Runner, private registration/token, explicit public upstream and pod-selector policy")
	}
	if c.cfg.Controller.EnableServices || c.cfg.Profiles["default"].Network == "source" || c.cfg.Profiles["source"].Network != "source" {
		t.Fatal("source gate requires isolated default, source profile, and disabled services")
	}
	validated, err := sourceproxy.New([]sourceproxy.Route{{Host: host, Port: 443, Address: address}})
	if err != nil {
		t.Fatal(err)
	}
	_ = validated.Close()
	ip, port, err := c.cfg.SourceProxyEndpoint()
	if err != nil || port != 3128 {
		t.Fatal("source gate requires reserved Service IP on TCP/3128")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	runnerData, err := os.ReadFile(runnerPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.admin.CoreV1().Secrets(c.cfg.Kubernetes.LeaseNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-runner", Namespace: c.cfg.Kubernetes.LeaseNamespace},
		Data:       map[string][]byte{"runner.yaml": runnerData},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	proxyManifest, err := os.ReadFile("../../../deploy/release/source-proxy.kubernetes.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	proxyManifest = []byte(strings.NewReplacer(
		"namespace: infra", "namespace: "+c.cfg.Kubernetes.LeaseNamespace,
		"kubernetes.io/metadata.name: ci", "kubernetes.io/metadata.name: "+c.cfg.Kubernetes.Namespace,
		"10.43.0.50", ip.String(), "git.dmedovich.com", host, "138.124.231.231", address,
		"registry.example.com/spawnpod@sha256:"+strings.Repeat("a", 64), c.cfg.AgentImage,
	).Replace(string(proxyManifest)))
	proxyManifest, caPath, fixtureID := c.sourceTLSFixture(t, proxyManifest, host, address)
	// Use a fixed-width random name even when the certificate serial has a
	// leading zero; the corresponding Actions secret is owned by this run.
	fixtureID = strings.Repeat("0", 32-len(fixtureID)) + fixtureID
	secretName := "RC7_OCI_" + strings.ToUpper(fixtureID)
	c.kubectl(t, proxyManifest, "apply", "-f", "-")
	c.kubectl(t, nil, "-n", c.cfg.Kubernetes.LeaseNamespace, "rollout", "status", "deployment/spawnpod-source-proxy", "--timeout=120s")
	bundle, err := installation.Build(c.cfg, "dev", c.cfg.AgentImage, runnerImage)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.RenderFiles(filepath.Join(c.evidence, "source-bundle"), false); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"10-network.yaml", "20-admission.yaml", "30-deployment.yaml"} {
		c.kubectl(t, bundle.Files[filename], "apply", "-f", "-")
	}
	t.Cleanup(func() {
		pods, err := c.admin.CoreV1().Pods(c.cfg.Kubernetes.LeaseNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=spawnpod"})
		if err != nil {
			t.Error(err)
			return
		}
		for _, pod := range pods.Items {
			for _, container := range []string{"controller", "runner"} {
				logs, err := c.admin.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container}).DoRaw(context.Background())
				if err == nil {
					_ = os.WriteFile(filepath.Join(c.evidence, "source-"+container+".log"), logs, 0600)
				}
			}
		}
	})
	c.kubectl(t, nil, "-n", c.cfg.Kubernetes.LeaseNamespace, "rollout", "status", "deployment/spawnpod", "--timeout=180s")
	workflowPath := filepath.Join(c.evidence, "workflow-rc7-profiles.yml")
	cmd := exec.CommandContext(ctx, "python3", "../make-source-workflow.py", "--proxy", c.cfg.SourceProxy, "--upstream-host", host, "--upstream-ip", address, "--output", workflowPath, "--ca-file", caPath, "--package-name", "rc7-transport-"+fixtureID, "--secret-name", secretName)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate source workflow: %v: %s", err, out)
	}
	cmd = exec.CommandContext(ctx, "python3", "../forgejo-workflow.py", "--token-file", tokenPath, "--workflow", workflowPath, "--package-secret", secretName)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 45 * time.Second
	output, err := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(c.evidence, "source-forgejo.log"), output, 0600)
	if err != nil {
		t.Fatalf("two-profile workflow failed; inspect private source-forgejo.log: %v", err)
	}
	records, err := c.store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]int{}
	for _, record := range records {
		profiles[record.Profile]++
	}
	if profiles["default"] != 1 || profiles["source"] != 1 || len(records) != 2 {
		t.Fatalf("unexpected Runner-selected profiles: %+v", profiles)
	}
	c.sourceRunnerCancellation(t, ctx, tokenPath, records)
	data, err := json.MarshalIndent(map[string]any{"passed": true, "profiles": profiles, "runnerImage": runnerImage, "runtimeImage": c.cfg.AgentImage, "scope": "real official Runner two labels, Exec proxy env, private Forgejo checkout, scoped OCI push/pull and cleanup, positive/negative HTTPS, executing source job cancellation and Pod cleanup"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.evidence, "source-runner-summary.json"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("official Runner executed default/source labels; records and network assertions passed")
}

// The trigger is written only after the controller records a real executing
// command in the new source environment, so cancellation cannot pass in queue.
func (c *cluster) sourceRunnerCancellation(t *testing.T, parent context.Context, tokenPath string, previous []engine.Record) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	workflow := filepath.Join(c.evidence, "workflow-rc7-source-cancel.yml")
	trigger := filepath.Join(c.evidence, "source-cancel.trigger")
	if err := os.WriteFile(workflow, []byte("name: RC7 source cancellation\non: workflow_dispatch\njobs:\n  source:\n    runs-on: spawnpod-source\n    timeout-minutes: 5\n    steps:\n      - name: Keep a real source command executing\n        run: node -e 'setTimeout(() => {}, 180000)'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	known := map[string]bool{}
	for _, record := range previous {
		known[record.ID] = true
	}
	cmd := exec.CommandContext(ctx, "python3", "../forgejo-workflow.py", "--token-file", tokenPath, "--workflow", workflow, "--cancel-trigger", trigger, "--expect-cancelled")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 45 * time.Second
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var executing engine.Record
	var waitErr error
	finished := false
	for !finished {
		select {
		case waitErr = <-done:
			finished = true
		case <-ctx.Done():
			waitErr = <-done
			finished = true
		case <-time.After(time.Second):
			if executing.ID != "" {
				continue
			}
			records, err := c.store.List(ctx)
			if err != nil {
				cancel()
				waitErr = <-done
				finished = true
				t.Error(err)
				continue
			}
			for _, record := range records {
				if !known[record.ID] && record.Profile == "source" && record.State == engine.StateExecuting && record.LastCommandID != "" {
					executing = record
					if err := os.WriteFile(trigger, []byte("executing source command observed\n"), 0600); err != nil {
						cancel()
						t.Error(err)
					}
					break
				}
			}
		}
	}
	_ = os.WriteFile(filepath.Join(c.evidence, "source-cancel-forgejo.log"), output.Bytes(), 0600)
	if waitErr != nil || executing.ID == "" {
		t.Fatalf("executing source cancellation failed: %v; inspect private source-cancel-forgejo.log", waitErr)
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		record, err := c.store.Get(ctx, executing.ID)
		if err == nil && record.State == engine.StateCleaned && !record.CleanedAt.IsZero() && record.LastCommandID == "" {
			_, err = c.admin.CoreV1().Pods(c.cfg.Kubernetes.Namespace).Get(ctx, executing.Job.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				t.Log("cancelled executing source command; cleaned record and absent job Pod verified")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("cancelled source environment did not reach cleaned state with its Pod absent")
}
