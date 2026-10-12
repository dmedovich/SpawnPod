package acceptance

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/installation"
	yaml "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The containerized path is an additional explicit opt-in. Source config must
// pin the imported current runtime; the previous image must match the previous
// release binary. It uses registered test Runner credentials without logging them.
func (c *cluster) bundleUpgrade(t *testing.T) {
	runnerImage := os.Getenv("SPAWNPOD_ACCEPTANCE_RUNNER_IMAGE")
	if runnerImage == "" {
		t.Skip("set SPAWNPOD_ACCEPTANCE_RUNNER_IMAGE, _RUNNER_CONFIG, _FORGEJO_TOKEN and _PREVIOUS_IMAGE for full containerized upgrade")
	}
	oldImage := os.Getenv("SPAWNPOD_ACCEPTANCE_PREVIOUS_IMAGE")
	runnerPath, tokenPath := os.Getenv("SPAWNPOD_ACCEPTANCE_RUNNER_CONFIG"), os.Getenv("SPAWNPOD_ACCEPTANCE_FORGEJO_TOKEN")
	if oldImage == "" || runnerPath == "" || tokenPath == "" {
		t.Fatal("incomplete containerized acceptance opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	runnerData, err := os.ReadFile(runnerPath)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := c.admin.CoreV1().Secrets(c.cfg.Kubernetes.LeaseNamespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "spawnpod-runner", Namespace: c.cfg.Kubernetes.LeaseNamespace}, Data: map[string][]byte{"runner.yaml": runnerData}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pods, e := c.admin.CoreV1().Pods(c.cfg.Kubernetes.LeaseNamespace).List(context.Background(), metav1.ListOptions{})
		if e != nil {
			return
		}
		for _, p := range pods.Items {
			for _, container := range []string{"controller", "runner"} {
				logs, e := c.admin.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: container}).DoRaw(context.Background())
				if e == nil {
					_ = os.WriteFile(filepath.Join(c.evidence, "bundle-final-"+container+".log"), logs, 0600)
				}
			}
		}
	})
	target, err := installation.Build(c.cfg, "dev", c.cfg.AgentImage, runnerImage)
	if err != nil {
		t.Fatal(err)
	}
	oldCfg := c.cfg
	oldCfg.AgentImage = oldImage
	oldCfg.Controller.TombstoneRetention = ""
	// RC6 provides the coordinated renderer; preserve its actual complete bundle.
	// Do not manufacture a legacy policy or rewrite mounted configuration.
	cfgData, err := yaml.Marshal(oldCfg)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(c.evidence, "previous-config.yaml")
	if err := os.WriteFile(oldPath, cfgData, 0600); err != nil {
		t.Fatal(err)
	}
	previousVersion, err := exec.Command(c.previous, "version").Output()
	if err != nil {
		t.Fatal(err)
	}
	oldDirectory := filepath.Join(c.evidence, "previous-bundle")
	render := exec.Command(c.previous, "upgrade", "render", "--config", oldPath, "--target", strings.TrimSpace(string(previousVersion)), "--image", oldImage, "--runner-image", runnerImage, "--output-dir", oldDirectory)
	if out, err := render.CombinedOutput(); err != nil {
		t.Fatalf("previous bundle render: %v: %s", err, out)
	}
	readPrevious := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(oldDirectory, name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	oldSecurity := readPrevious("00-security.yaml")
	oldNetwork := readPrevious("10-network.yaml")
	oldPolicy := readPrevious("20-admission.yaml")
	oldDeployment := readPrevious("30-deployment.yaml")
	imp := rest.CopyConfig(c.rest)
	imp.Impersonate.UserName = "system:serviceaccount:" + c.cfg.Kubernetes.LeaseNamespace + ":spawnpod-controller"
	imp.Impersonate.Groups = []string{"system:serviceaccounts", "system:serviceaccounts:" + c.cfg.Kubernetes.LeaseNamespace, "system:authenticated"}
	restricted, err := kubernetes.NewForConfig(imp)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(data []byte) { c.kubectl(t, data, "apply", "-f", "-") }
	wait := func(name string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for !condition() {
			if time.Now().After(deadline) {
				pods, _ := c.admin.CoreV1().Pods(c.cfg.Kubernetes.LeaseNamespace).List(ctx, metav1.ListOptions{})
				for _, p := range pods.Items {
					for _, container := range []string{"controller", "runner"} {
						logs, e := c.admin.CoreV1().Pods(p.Namespace).GetLogs(p.Name, &corev1.PodLogOptions{Container: container}).DoRaw(ctx)
						if e == nil {
							_ = os.WriteFile(filepath.Join(c.evidence, name+"-"+container+".log"), logs, 0600)
						}
					}
				}
				t.Fatalf("timeout waiting for %s", name)
			}
			time.Sleep(time.Second)
		}
	}
	ready := func() {
		command := exec.Command("kubectl", "--kubeconfig", c.cfg.Kubernetes.Kubeconfig, "--request-timeout=20s", "-n", c.cfg.Kubernetes.LeaseNamespace, "rollout", "status", "deployment/spawnpod", "--timeout=180s")
		output, err := command.CombinedOutput()
		if err == nil {
			return
		}
		pods, _ := c.admin.CoreV1().Pods(c.cfg.Kubernetes.LeaseNamespace).List(ctx, metav1.ListOptions{})
		if pods != nil {
			for _, pod := range pods.Items {
				for _, status := range pod.Status.ContainerStatuses {
					t.Logf("pod %s container %s ready=%v waiting=%v terminated=%v", pod.Name, status.Name, status.Ready, status.State.Waiting, status.State.Terminated)
				}
				events, _ := c.admin.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(pod.UID)})
				if events != nil {
					for _, event := range events.Items {
						if event.Type == "Warning" {
							t.Logf("pod %s event %s: %s", pod.Name, event.Reason, event.Message)
						}
					}
				}
				// Controller never receives Runner credentials; keep Runner logs private.
				tail := int64(20)
				logs, e := c.admin.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: "controller", TailLines: &tail}).DoRaw(ctx)
				if e == nil {
					t.Logf("controller diagnostic: %s", logs)
				}
			}
		}
		t.Fatalf("deployment readiness: %v: %s", err, output)
	}
	drainAndStop := func() {
		wait("workload-cleanup", func() bool {
			pods, e := c.admin.CoreV1().Pods(c.cfg.Kubernetes.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "spawnpod.dev/managed=true"})
			return e == nil && len(pods.Items) == 0
		})
		c.kubectl(t, nil, "-n", c.cfg.Kubernetes.LeaseNamespace, "scale", "deployment/spawnpod", "--replicas=0")
		wait("installation-stopped", func() bool {
			pods, e := c.admin.CoreV1().Pods(c.cfg.Kubernetes.LeaseNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=spawnpod"})
			return e == nil && len(pods.Items) == 0
		})
	}
	smoke := func(label string) {
		cmd := exec.Command("python3", "../forgejo-workflow.py", "--token-file", tokenPath, "--workflow", "../../../deploy/release/workflow-smoke.yml")
		out, err := cmd.CombinedOutput()
		_ = os.WriteFile(filepath.Join(c.evidence, label+"-forgejo.log"), out, 0600)
		if err != nil {
			t.Fatalf("Forgejo smoke failed (%s); see private evidence: %v", label, err)
		}
	}
	apply(oldSecurity)
	apply(oldNetwork)
	apply(oldPolicy)
	apply(oldDeployment)
	ready()
	smoke("previous")
	recordsBefore, err := c.store.List(ctx)
	if err != nil || len(recordsBefore) == 0 {
		t.Fatalf("missing real CI records: %v", err)
	}
	drainAndStop()
	report := target.Check(ctx, c.admin, restricted, "switch")
	if report.Err() == nil {
		t.Fatal("stale policy allowed target switch")
	}
	apply(target.Files["00-security.yaml"])
	apply(target.Files["10-network.yaml"])
	apply(target.Files["20-admission.yaml"])
	wait("target-preflight", func() bool { return target.Check(ctx, c.admin, restricted, "switch").Err() == nil })
	// Repeat after a simulated interruption at the dependency gate.
	apply(target.Files["20-admission.yaml"])
	apply(target.Files["30-deployment.yaml"])
	ready()
	wait("target-ready", func() bool { return target.Check(ctx, c.admin, restricted, "ready").Err() == nil })
	smoke("target")
	drainAndStop()
	apply(target.Files["30-deployment.yaml"])
	ready()
	d, err := c.admin.AppsV1().Deployments(c.cfg.Kubernetes.LeaseNamespace).Get(ctx, "spawnpod", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d.Spec.Template.Spec.Containers[0].Args = []string{"serve", "--config", "/intentional-missing-config"}
	if _, err := c.admin.AppsV1().Deployments(d.Namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait("intentional-failure", func() bool {
		pods, e := c.admin.CoreV1().Pods(d.Namespace).List(ctx, metav1.ListOptions{})
		if e != nil {
			return false
		}
		for _, p := range pods.Items {
			for _, s := range p.Status.ContainerStatuses {
				if s.Name == "controller" && s.State.Waiting != nil && s.RestartCount > 0 {
					return true
				}
			}
		}
		return false
	})
	if r := target.Check(ctx, c.admin, restricted, "ready"); r.Err() == nil || r.CIProven {
		t.Fatal("broken rollout reported success")
	}
	drainAndStop()
	apply(oldSecurity)
	apply(oldNetwork)
	apply(oldPolicy)
	apply(oldDeployment)
	ready()
	smoke("rollback")
	restored, err := c.admin.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil || restored.UID != secret.UID || restored.ResourceVersion != secret.ResourceVersion || !bytes.Equal(restored.Data["runner.yaml"], runnerData) {
		t.Fatal("Runner Secret changed during upgrade/rollback")
	}
	for _, record := range recordsBefore {
		current, e := c.store.Get(ctx, record.ID)
		if e != nil || current.ID != record.ID {
			t.Fatalf("record lost across upgrade: %s %v", record.ID, e)
		}
	}
	drainAndStop()
	apply(target.Files["10-network.yaml"])
	apply(target.Files["20-admission.yaml"])
	wait("retry-preflight", func() bool { return target.Check(ctx, c.admin, restricted, "switch").Err() == nil })
	apply(target.Files["30-deployment.yaml"])
	ready()
	wait("retry-ready", func() bool { return target.Check(ctx, c.admin, restricted, "ready").Err() == nil })
	smoke("retry")
	t.Log("containerized previous -> target -> intentional failure -> full rollback -> target; real Forgejo jobs passed; Secret UID/RV and records preserved")
	// Stop containers before parent cleanup removes namespaces/restores policy.
	drainAndStop()
}
