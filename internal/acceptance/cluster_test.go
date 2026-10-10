// Package acceptance exercises a local K3s using real release binaries and RPCs.
// It is opt-in; ordinary go test never creates cluster resources.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pb "github.com/dmedovich/spawnpod/internal/adapter/forgejo/proto"
	"github.com/dmedovich/spawnpod/internal/admission"
	"github.com/dmedovich/spawnpod/internal/config"
	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	"github.com/dmedovich/spawnpod/internal/smoke"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

type cluster struct {
	controllerProcess                       *os.Process
	cfg                                     config.Config
	configPath, current, previous, evidence string
	admin                                   kubernetes.Interface
	rest                                    *rest.Config
	jobs                                    *provider.EngineJobs
	store                                   *provider.ConfigMapStore
	backend                                 *engine.Engine
}

func TestLocalClusterAcceptance(t *testing.T) {
	configPath := os.Getenv("SPAWNPOD_ACCEPTANCE_CONFIG")
	if configPath == "" {
		t.Skip("set SPAWNPOD_ACCEPTANCE_CONFIG to explicitly opt into local K3s acceptance")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.Kubernetes.Namespace, "sp-acceptance-") || !strings.HasPrefix(cfg.Kubernetes.LeaseNamespace, "sp-acceptance-") {
		t.Fatal("acceptance requires disposable sp-acceptance-* namespaces")
	}
	rc, err := clientcmd.BuildConfigFromFlags("", cfg.Kubernetes.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(rc.Host)
	if err != nil {
		t.Fatal(err)
	}
	address := net.ParseIP(endpoint.Hostname())
	if address == nil || !address.IsLoopback() {
		t.Fatal("acceptance refuses non-loopback clusters")
	}
	rc.Timeout = 15 * time.Second
	admin, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{cfg: cfg, configPath: configPath, current: os.Getenv("SPAWNPOD_CONTROLLER_CURRENT"), previous: os.Getenv("SPAWNPOD_CONTROLLER_PREVIOUS"), evidence: filepath.Dir(configPath), admin: admin, rest: rc}
	if c.current == "" || c.previous == "" {
		t.Fatal("both current and previous release binaries are required")
	}
	c.install(t)
	imp := rest.CopyConfig(rc)
	imp.Impersonate.UserName = "system:serviceaccount:" + cfg.Kubernetes.LeaseNamespace + ":spawnpod-controller"
	imp.Impersonate.Groups = []string{"system:serviceaccounts", "system:serviceaccounts:" + cfg.Kubernetes.LeaseNamespace, "system:authenticated"}
	impClient, err := kubernetes.NewForConfig(imp)
	if err != nil {
		t.Fatal(err)
	}
	p := &provider.Provider{Client: impClient, Config: imp, Namespace: cfg.Kubernetes.Namespace}
	c.jobs = &provider.EngineJobs{Provider: p}
	c.store = &provider.ConfigMapStore{Client: impClient, Namespace: cfg.Kubernetes.Namespace}
	c.backend = &engine.Engine{Store: c.store, Jobs: c.jobs, Services: &provider.EngineServices{Provider: p}, CleanupTimeout: cleanupDuration(cfg)}
	t.Run("AdmissionAndRBAC", c.admission)
	t.Run("NetworkAndGuard", c.network)
	t.Run("Capacity", c.capacity)
	t.Run("APIOutage", c.apiOutage)
	t.Run("ControllerCrash", c.crash)
	t.Run("RestartStates", c.recovery)
	t.Run("ReleasedControllerUpgradeRollback", c.upgrade)
}

func (c *cluster) kubectl(t *testing.T, input []byte, args ...string) []byte {
	t.Helper()
	command := exec.Command("kubectl", append([]string{"--kubeconfig", c.cfg.Kubernetes.Kubeconfig, "--request-timeout=20s"}, args...)...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %v: %v: %s", args, err, output)
	}
	return output
}

func (c *cluster) install(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// Never replace admission while a pre-existing SpawnPod workload is active.
	existing, err := c.admin.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "spawnpod.dev/managed=true"})
	if err != nil {
		t.Fatal(err)
	}
	if len(existing.Items) != 0 {
		t.Fatal("stop existing SpawnPod workloads before acceptance")
	}
	for _, ns := range []string{c.cfg.Kubernetes.Namespace, c.cfg.Kubernetes.LeaseNamespace} {
		if _, err := c.admin.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("test namespace already exists or lookup failed: %s %v", ns, err)
		}
	}
	saved, err := c.admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, admission.PolicyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(saved, "", "  ")
	if err := os.WriteFile(filepath.Join(c.evidence, "original-admission.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// Register recovery before any mutation. Keep the original admission object UID.
	t.Cleanup(func() {
		for _, ns := range []string{c.cfg.Kubernetes.Namespace, c.cfg.Kubernetes.LeaseNamespace} {
			c.kubectl(t, nil, "delete", "namespace", ns, "--ignore-not-found", "--wait=true", "--timeout=60s")
		}
		name := c.cfg.Kubernetes.Namespace + "-admission-read"
		c.kubectl(t, nil, "delete", "clusterrolebinding", name, "--ignore-not-found")
		c.kubectl(t, nil, "delete", "clusterrole", name, "--ignore-not-found")
		current, err := c.admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(ctx, admission.PolicyName, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		if current.UID != saved.UID {
			t.Error("admission UID changed; refusing automatic restore")
			return
		}
		current.Spec = saved.Spec
		if _, err := c.admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	})
	security, err := os.ReadFile("../../deploy/security.yaml")
	if err != nil {
		t.Fatal(err)
	}
	translated := strings.NewReplacer(": ci\n", ": "+c.cfg.Kubernetes.Namespace+"\n", ": infra\n", ": "+c.cfg.Kubernetes.LeaseNamespace+"\n", "[ci]", "["+c.cfg.Kubernetes.Namespace+"]", "name: spawnpod-admission-read", "name: "+c.cfg.Kubernetes.Namespace+"-admission-read").Replace(string(security))
	c.kubectl(t, []byte(translated), "apply", "-f", "-")
	policy, _, err := admission.Objects(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	updatedPolicy := saved.DeepCopy()
	updatedPolicy.Spec = policy.Spec
	if _, err := c.admin.AdmissionregistrationV1().ValidatingAdmissionPolicies().Update(ctx, updatedPolicy, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	limits, err := admission.ContainerLimits(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	quota, err := admission.NamespaceQuota(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	objects := corev1.List{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "List"}}
	for _, obj := range []any{limits, quota} {
		data, _ := json.Marshal(obj)
		objects.Items = append(objects.Items, k8sruntime.RawExtension{Raw: data})
	}
	data, _ = json.Marshal(objects)
	c.kubectl(t, data, "apply", "-f", "-")
	deny := provider.BuildDefaultDenyPolicy()
	deny.Namespace = c.cfg.Kubernetes.Namespace
	if _, err := c.admin.NetworkingV1().NetworkPolicies(deny.Namespace).Create(ctx, deny, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Wait until admission sees the changed policy, not merely its API write.
	deadline := time.Now().Add(20 * time.Second)
	for {
		pod := c.pod(t, strings.Repeat("a", 32))
		_, err := c.admin.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (c *cluster) pod(t *testing.T, id string) *corev1.Pod {
	t.Helper()
	spec := c.cfg.Profiles["default"].JobSpec(c.cfg.AgentImage)
	spec.EnvironmentID = id
	pod, err := provider.BuildJobPod(spec)
	if err != nil {
		t.Fatal(err)
	}
	pod.Namespace = c.cfg.Kubernetes.Namespace
	return pod
}

// Kubernetes returns CEL validation failures as 422/Invalid even when the
// message says forbidden. PodSecurity rejects the host/privilege cases first.
func isAdmissionDenial(err error) bool {
	if err == nil {
		return false
	}
	return (apierrors.IsInvalid(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy 'spawnpod-workload'")) ||
		(apierrors.IsForbidden(err) && strings.Contains(err.Error(), "violates PodSecurity"))
}

func (c *cluster) admission(t *testing.T) {
	ctx := context.Background()
	changes := map[string]func(*corev1.Pod){
		"unapproved-image": func(p *corev1.Pod) { p.Spec.Containers[0].Image = "docker.io/library/alpine:latest" },
		"privileged": func(p *corev1.Pod) {
			v := true
			p.Spec.Containers[0].SecurityContext.Privileged = &v
			p.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = &v
		},
		"host-network": func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"host-path": func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc"}}})
		},
		"secret-volume": func(p *corev1.Pod) {
			p.Spec.Volumes[0].EmptyDir = nil
			p.Spec.Volumes[0].Secret = &corev1.SecretVolumeSource{SecretName: "canary"}
		},
		"secret-env": func(p *corev1.Pod) {
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "canary"}, Key: "token"}}}}
		},
		"token-mount":          func(p *corev1.Pod) { v := true; p.Spec.AutomountServiceAccountToken = &v },
		"different-entrypoint": func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 30"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			pod := c.pod(t, strings.Repeat("a", 32))
			change(pod)
			if _, err := c.admin.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); !isAdmissionDenial(err) {
				t.Fatalf("expected admission denial, got %v", err)
			}
		})
	}
	identity := "system:serviceaccount:" + c.cfg.Kubernetes.LeaseNamespace + ":spawnpod-controller"
	for _, check := range []struct{ verb, resource, namespace, want string }{{"create", "pods", c.cfg.Kubernetes.Namespace, "yes"}, {"get", "secrets", c.cfg.Kubernetes.Namespace, "no"}, {"create", "pods", "default", "no"}} {
		command := exec.Command("kubectl", "--kubeconfig", c.cfg.Kubernetes.Kubeconfig, "auth", "can-i", check.verb, check.resource, "-n", check.namespace, "--as="+identity)
		output, err := command.CombinedOutput()
		if strings.TrimSpace(string(output)) != check.want || (check.want == "yes" && err != nil) {
			t.Fatalf("RBAC %v: %s %v", check, output, err)
		}
	}
}

func (c *cluster) create(t *testing.T, services bool) engine.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	req := engine.CreateRequest{Owner: "acceptance", Profile: "default", Deadline: time.Now().Add(10 * time.Minute), Job: c.cfg.Profiles["default"].JobSpec(c.cfg.AgentImage)}
	if services {
		req.Services = []engine.ServiceSpec{c.cfg.Profiles["default"].Services["echo"].Spec("echo", c.cfg.AgentImage)}
	}
	record, err := c.backend.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := c.backend.Remove(ctx, record.ID); err != nil {
			t.Error(err)
		}
		current, err := c.store.Get(ctx, record.ID)
		if err == nil && current.State == engine.StateCleaned {
			if err := c.store.Delete(ctx, record.ID, current.Revision); err != nil {
				t.Error(err)
			}
		}
	})
	record, err = c.backend.Start(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (c *cluster) command(t *testing.T, record engine.Record, argv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	var output bytes.Buffer
	code := -999
	req := supervisor.Request{Op: "run", ID: fmt.Sprintf("check-%d", time.Now().UnixNano()), Argv: argv, Dir: "/workspace", TimeoutMillis: 60000}
	_, err := c.backend.Exec(ctx, record.ID, req, func(ev supervisor.Event) error {
		if ev.Stream == "stdout" {
			output.Write(ev.Data)
		}
		if ev.ExitCode != nil {
			code = *ev.ExitCode
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return output.String(), code
}

func (c *cluster) controlExec(t *testing.T, pod *corev1.Pod, argv ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req := c.admin.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("pods").Name(pod.Name).SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{Container: pod.Spec.Containers[0].Name, Command: argv, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(c.rest, "POST", req.URL())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
	return stdout.String(), err
}

func (c *cluster) control(t *testing.T) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	// A positive control has no NetworkPolicy and is outside the workload namespace.
	pod := c.pod(t, strings.Repeat("f", 32))
	pod.Name = "network-control"
	pod.Namespace = c.cfg.Kubernetes.LeaseNamespace
	pod.Labels = nil
	pod.Spec.InitContainers = nil
	pod.Spec.ServiceAccountName = "default"
	pod.Spec.Containers[0].Name = "control"
	pod.Spec.Containers[0].Command = []string{"node", "-e", "require('http').createServer((q,r)=>r.end('control-ok')).listen(8080,'0.0.0.0')"}
	pod.Spec.Containers[0].ReadinessProbe = nil
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}}
	pod.Spec.Volumes = []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	pod, err := c.admin.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.admin.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(time.Minute)
	for {
		pod, err = c.admin.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("control not ready: %s", pod.Status.Phase)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for {
		out, err := c.controlExec(t, pod, "busybox", "wget", "-qO-", "-T", "2", "http://127.0.0.1:8080")
		if err == nil && out == "control-ok" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control HTTP server not ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return pod
}

func (c *cluster) network(t *testing.T) {
	a := c.create(t, true)
	b := c.create(t, false)
	control := c.control(t)
	api, err := c.admin.CoreV1().Services("default").Get(context.Background(), "kubernetes", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.controlExec(t, control, "busybox", "nc", "-z", "-w", "2", api.Spec.ClusterIP, "443"); err != nil {
		t.Fatalf("positive API control failed: %v", err)
	}
	serviceIP := a.Services[0].ClusterIP
	out, code := c.command(t, a, "busybox", "wget", "-qO-", "-T", "3", "http://"+serviceIP+":8080")
	if code != 0 || out != "service-ok" {
		t.Fatalf("same-environment service inaccessible: %q exit=%d", out, code)
	}
	for name, target := range map[string][]string{
		"api-egress":                {api.Spec.ClusterIP, "443"},
		"unapproved-pod-egress":     {control.Status.PodIP, "8080"},
		"other-environment-service": {serviceIP, "8080"},
	} {
		t.Run(name, func(t *testing.T) {
			_, code := c.command(t, b, "busybox", "nc", "-z", "-w", "2", target[0], target[1])
			if code == 0 {
				t.Fatal("forbidden connection succeeded")
			}
		})
	}
	if _, err := c.controlExec(t, control, "busybox", "nc", "-z", "-w", "2", serviceIP, "8080"); err == nil {
		t.Fatal("service ingress from unrelated Pod succeeded")
	}
	// Recheck the permitted path so endpoint failure cannot explain a denial.
	out, code = c.command(t, a, "busybox", "wget", "-qO-", "-T", "3", "http://"+serviceIP+":8080")
	if code != 0 || out != "service-ok" {
		t.Fatal("service positive control disappeared")
	}
	_, code = c.command(t, b, "sh", "-c", "setsid -w sh -c 'touch /workspace/escaped' >/tmp/setsid.log 2>&1; test ! -e /workspace/escaped")
	if code != 0 {
		t.Fatal("session escape succeeded")
	}
	_, code = c.command(t, b, "sh", "-c", "test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token; git init -q /workspace/repo; node -e \"const r=require('child_process').spawnSync('sh',['-c','exit 0']);process.exit(r.status)\"")
	if code != 0 {
		t.Fatal("tokenless Git/Node compatibility check failed")
	}
}

func (c *cluster) capacity(t *testing.T) {
	stop := c.startController(t, c.current, "capacity-controller")
	defer stop()
	client := c.rpc(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var ids []string
	defer func() {
		for _, id := range ids {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, err := client.Remove(cleanup, &pb.RemoveRequest{EnvironmentId: id})
			cancel()
			if err != nil {
				t.Error(err)
			}
		}
	}()
	for i := 0; i < 3; i++ {
		record, err := client.Create(ctx, &pb.CreateRequest{Name: "capacity", Image: c.cfg.Profiles["default"].Image, BackendOptions: map[string]string{"profile": "default"}, EnvironmentTimeout: durationpb.New(10 * time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, record.GetEnvironmentId())
		started, err := client.Start(ctx, &pb.StartRequest{EnvironmentId: record.GetEnvironmentId()})
		if err != nil {
			t.Fatal(err)
		}
		for {
			_, err = started.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	idle, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", c.controllerProcess.Pid))
	if err != nil {
		t.Fatal(err)
	}
	idleCPU := c.cpuSeconds(t)
	loadStart := time.Now()
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			stream, err := client.Exec(ctx, &pb.ExecRequest{EnvironmentId: id, Command: []string{"node", "-e", "const end=Date.now()+90000; const b=Buffer.alloc(16*1024*1024,1); while(Date.now()<end){for(let i=0;i<b.length;i+=4096)b[i]^=1}"}})
			if err != nil {
				t.Error(err)
				return
			}
			complete := false
			for {
				message, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Error(err)
					return
				}
				if result := message.GetExecComplete(); result != nil {
					complete = result.ExitCode == 0
				}
				if message.GetExecFailed() != nil {
					t.Error("capacity command failed")
				}
			}
			if !complete {
				t.Error("capacity command did not complete successfully")
			}
		}(id)
	}
	time.Sleep(35 * time.Second)
	var metrics []byte
	deadline := time.Now().Add(35 * time.Second)
	for {
		command := exec.Command("kubectl", "--kubeconfig", c.cfg.Kubernetes.Kubeconfig, "top", "pods", "-n", c.cfg.Kubernetes.Namespace, "--containers")
		metrics, err = command.CombinedOutput()
		if err == nil && loadedMetrics(metrics) {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("metrics unavailable: %v %s", err, metrics)
			break
		}
		time.Sleep(time.Second)
	}
	loaded, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", c.controllerProcess.Pid))
	if err != nil {
		t.Error(err)
	}
	report := append([]byte(fmt.Sprintf("CONCURRENCY=3; NODE workload; per-job CPU limit=500m; memory limit=256Mi\nController average CPU during load: %.4f cores over %.1fs\n", (c.cpuSeconds(t)-idleCPU)/time.Since(loadStart).Seconds(), time.Since(loadStart).Seconds())), metrics...)
	report = append(report, []byte("\nCONTROLLER IDLE\n")...)
	report = append(report, idle...)
	report = append(report, []byte("\nCONTROLLER UNDER LOAD\n")...)
	report = append(report, loaded...)
	if err := os.WriteFile(filepath.Join(c.evidence, "capacity.txt"), report, 0600); err != nil {
		t.Error(err)
	}
	t.Logf("local concurrency=3:\n%s", metrics)
	wg.Wait()
}

// Require every sample to include the allocated buffer, not pre-execution RSS.
func loadedMetrics(metrics []byte) bool {
	rows := 0
	for _, line := range strings.Split(string(metrics), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 || !strings.HasPrefix(fields[0], "sp-") {
			continue
		}
		memory, err := strconv.Atoi(strings.TrimSuffix(fields[3], "Mi"))
		if err != nil || memory < 16 {
			return false
		}
		rows++
	}
	return rows == 3
}

func (c *cluster) cpuSeconds(t *testing.T) float64 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", c.controllerProcess.Pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
	user, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		t.Fatal(err)
	}
	system, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		t.Fatal(err)
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(string(ticks)), 64)
	if err != nil || hz <= 0 {
		t.Fatalf("invalid CLK_TCK: %q %v", ticks, err)
	}
	return (user + system) / hz
}

func (c *cluster) recovery(t *testing.T) {
	for _, state := range []engine.State{engine.StateProvisioning, engine.StateReady, engine.StateExecuting, engine.StateTransferring, engine.StateFailed, engine.StateIndeterminate, engine.StateTerminating, engine.StateCleaned} {
		t.Run(string(state), func(t *testing.T) {
			record := c.create(t, false)
			record.State = state
			if state == engine.StateCleaned {
				record.CleanedAt = time.Now()
			}
			record, err := c.store.Update(context.Background(), record)
			if err != nil {
				t.Fatal(err)
			}
			fresh := &engine.Engine{Store: c.store, Jobs: c.jobs, CleanupTimeout: cleanupDuration(c.cfg)}
			if err := fresh.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			current, err := c.store.Get(context.Background(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := engine.StateCleaned
			if state == engine.StateReady {
				want = engine.StateReady
			}
			if current.State != want {
				t.Fatalf("recovery state=%s want=%s", current.State, want)
			}
			if state != engine.StateReady && state != engine.StateCleaned {
				if _, err := c.admin.CoreV1().Pods(c.cfg.Kubernetes.Namespace).Get(context.Background(), record.Job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
					t.Fatalf("recovery left Pod: %v", err)
				}
			}
			// StateCleaned means resources were already deleted before persisting the tombstone.
			if state == engine.StateCleaned {
				if err := c.jobs.RemoveJob(context.Background(), record.Job); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func (c *cluster) startController(t *testing.T, binary, label string) func() {
	t.Helper()
	log, err := os.Create(filepath.Join(c.evidence, label+".log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "serve", "--config", c.configPath, "--as-controller")
	command.Stdout = log
	command.Stderr = log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	c.controllerProcess = command.Process
	done := make(chan error, 1)
	go func() { done <- command.Wait(); log.Close() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(35 * time.Second):
				_ = command.Process.Kill()
				<-done
				t.Error("controller shutdown exceeded bound")
			}
		})
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(45 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", c.cfg.Controller.SocketPath, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return stop
		}
		select {
		case err := <-done:
			done <- err
			t.Fatalf("controller startup failed: %v; inspect %s.log", err, label)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("controller socket did not appear")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *cluster) rpc(t *testing.T) pb.BackendPluginClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///acceptance", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.WaitForReady(true)), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", c.cfg.Controller.SocketPath)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewBackendPluginClient(conn)
}

func (c *cluster) upgrade(t *testing.T) {
	stopOld := c.startController(t, c.previous, "old-controller")
	client := c.rpc(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	created, err := client.Create(ctx, &pb.CreateRequest{Name: "upgrade", Image: c.cfg.Profiles["default"].Image, BackendOptions: map[string]string{"profile": "default"}, EnvironmentTimeout: durationpb.New(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Start(ctx, &pb.StartRequest{EnvironmentId: created.GetEnvironmentId()})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	id := created.GetEnvironmentId()
	before, err := c.store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Remove(ctx, &pb.RemoveRequest{EnvironmentId: id}); err != nil {
		t.Fatal(err)
	}
	stopOld()
	stopNew := c.startController(t, c.current, "new-controller")
	after, err := c.store.Get(ctx, id)
	if err != nil || before.ID != after.ID || before.Job != after.Job || after.State != engine.StateCleaned {
		t.Fatalf("upgrade changed ready record: %+v %v", after, err)
	}
	if err := smoke.Run(ctx, c.cfg.Controller.SocketPath, c.cfg.Profiles["default"].Image, "default", io.Discard); err != nil {
		t.Fatal(err)
	}
	// New controller produces a service record (v2), then old controller reads it on rollback.
	service, err := client.Create(ctx, &pb.CreateRequest{Name: "rollback-service", Image: c.cfg.Profiles["default"].Image, BackendOptions: map[string]string{"profile": "default"}, Services: []*pb.ServiceContainer{{Name: "echo", Image: c.cfg.Profiles["default"].Services["echo"].Image, Ports: []string{"8080:8080"}}}, EnvironmentTimeout: durationpb.New(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	stream, err = client.Start(ctx, &pb.StartRequest{EnvironmentId: service.GetEnvironmentId()})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.Remove(ctx, &pb.RemoveRequest{EnvironmentId: service.GetEnvironmentId()}); err != nil {
		t.Fatal(err)
	}
	stopNew()
	stopRollback := c.startController(t, c.previous, "rollback-controller")
	if record, err := c.store.Get(ctx, service.GetEnvironmentId()); err != nil || record.Version != 2 || record.State != engine.StateCleaned {
		t.Fatalf("rollback record: %+v %v", record, err)
	}
	for _, id := range []string{id, service.GetEnvironmentId()} {
		if _, err := client.Remove(ctx, &pb.RemoveRequest{EnvironmentId: id}); err != nil {
			t.Fatal(err)
		}
	}
	stopRollback()
	t.Log("rc.1 -> current -> rc.1: drained v1/v2 records retained; current package RPC smoke passed")
}

func cleanupDuration(cfg config.Config) time.Duration { value, _ := cfg.CleanupTimeout(); return value }

type faultState struct{ armed, blocked atomic.Bool }
type faultTransport struct {
	next http.RoundTripper
	*faultState
}

func (f *faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.blocked.Load() {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if f.armed.Load() && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/exec") {
		f.blocked.Store(true)
		return nil, errors.New("injected controller transport outage")
	}
	return f.next.RoundTrip(req)
}

func (c *cluster) apiOutage(t *testing.T) {
	record := c.create(t, false)
	rc := rest.CopyConfig(c.jobs.Provider.Config)
	fault := &faultState{}
	previous := rc.WrapTransport
	rc.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		if previous != nil {
			next = previous(next)
		}
		return &faultTransport{next: next, faultState: fault}
	}
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	faultyProvider := &provider.Provider{Client: client, Config: rc, Namespace: c.cfg.Kubernetes.Namespace}
	faulty := &engine.Engine{Store: &provider.ConfigMapStore{Client: client, Namespace: c.cfg.Kubernetes.Namespace}, Jobs: &provider.EngineJobs{Provider: faultyProvider}, CleanupTimeout: time.Second}
	fault.armed.Store(true)
	defer fault.armed.Store(false)
	defer fault.blocked.Store(false)
	done := make(chan error, 1)
	go func() {
		done <- faulty.CopyIn(context.Background(), record.ID, "/workspace", strings.NewReader("archive"))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("outage did not fail transfer")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("API outage held operation past cleanup bound")
	}
	if !fault.blocked.Load() {
		t.Fatal("fault never intercepted the real pods/exec request")
	}
	current, err := c.store.Get(context.Background(), record.ID)
	if err != nil || current.State != engine.StateTransferring {
		t.Fatalf("durable interrupted intent lost: %+v %v", current, err)
	}
	fault.armed.Store(false)
	fault.blocked.Store(false)
	recovered := &engine.Engine{Store: c.store, Jobs: c.jobs, CleanupTimeout: cleanupDuration(c.cfg)}
	if err := recovered.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err = c.store.Get(context.Background(), record.ID)
	if err != nil || current.State != engine.StateCleaned {
		t.Fatalf("outage recovery failed: %+v %v", current, err)
	}
	t.Log("controller-only HTTP transport outage after durable transfer intent; bounded return and fresh-engine cleanup passed; K3s itself was not stopped")
}

func (c *cluster) crash(t *testing.T) {
	stop := c.startController(t, c.current, "crash-controller")
	client := c.rpc(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	created, err := client.Create(ctx, &pb.CreateRequest{Name: "crash", Image: c.cfg.Profiles["default"].Image, BackendOptions: map[string]string{"profile": "default"}, EnvironmentTimeout: durationpb.New(10 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	started, err := client.Start(ctx, &pb.StartRequest{EnvironmentId: created.GetEnvironmentId()})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = started.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	execution, err := client.Exec(ctx, &pb.ExecRequest{EnvironmentId: created.GetEnvironmentId(), Command: []string{"sh", "-c", "printf running; sleep 600"}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := execution.Recv()
	if err != nil || event.GetData() == nil || string(event.GetData().Data) != "running" {
		t.Fatalf("command not running: %v %v", event, err)
	}
	if err := c.controllerProcess.Kill(); err != nil {
		t.Fatal(err)
	}
	stop()
	record, err := c.store.Get(ctx, created.GetEnvironmentId())
	if err != nil || record.State != engine.StateExecuting {
		t.Fatalf("crash intent missing: %+v %v", record, err)
	}
	stopRecovered := c.startController(t, c.current, "recovered-controller")
	defer stopRecovered()
	record, err = c.store.Get(ctx, created.GetEnvironmentId())
	if err != nil || record.State != engine.StateCleaned {
		t.Fatalf("restart did not reconcile crashed execution: %+v %v", record, err)
	}
	if _, err := c.admin.CoreV1().Pods(c.cfg.Kubernetes.Namespace).Get(ctx, record.Job.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("crashed job Pod survived recovery: %v", err)
	}
}
