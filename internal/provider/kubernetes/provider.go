package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dmedovich/spawnpod/internal/supervisor"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Provider owns Kubernetes transport and resource identity checks. It does not
// persist environment state; callers must record the returned Pod UID.
type Provider struct {
	Client    kubernetes.Interface
	Config    *rest.Config
	Namespace string
}

func (p *Provider) validate() error {
	if p == nil || p.Client == nil || p.Namespace == "" {
		return errors.New("kubernetes provider is not configured")
	}
	return nil
}

// CreateJob creates the Pod and returns its server-assigned UID. If the API
// response is lost, callers must Get the deterministic name and verify labels
// and the intended spec before assuming the Pod belongs to this request.
func (p *Provider) CreateJob(ctx context.Context, s JobSpec) (*corev1.Pod, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	pod, err := BuildJobPod(s)
	if err != nil {
		return nil, err
	}
	return p.Client.CoreV1().Pods(p.Namespace).Create(ctx, pod, metav1.CreateOptions{})
}

// WaitReady polls the Pod until the agent readiness probe succeeds or ctx
// expires. It reports terminal Pod failures instead of waiting indefinitely.
func (p *Provider) WaitReady(ctx context.Context, name string, uid types.UID) (*corev1.Pod, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if pod.UID != uid {
			return nil, errors.New("pod UID changed while waiting for readiness")
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return pod, nil
			}
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return nil, fmt.Errorf("job Pod finished before readiness: %s", pod.Status.Phase)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// RemoveJob deletes only the Pod with the recorded UID. NotFound is a success
// for a previously recorded environment, but no other deletion error is hidden.
func (p *Provider) RemoveJob(ctx context.Context, name string, uid types.UID) error {
	if err := p.validate(); err != nil {
		return err
	}
	if uid == "" {
		return errors.New("pod UID is required for deletion")
	}
	zero := int64(0)
	err := p.Client.CoreV1().Pods(p.Namespace).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}, GracePeriodSeconds: &zero})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, getErr := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return nil
		}
		if getErr != nil {
			return getErr
		}
		if pod.UID != uid {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// CancelCommand asks the agent to stop one command. A successful response is
// only an acknowledgement; callers must observe Exec ending or delete the Pod
// within their cleanup deadline.
func (p *Provider) CancelCommand(ctx context.Context, name string, uid types.UID, commandID string) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Config == nil {
		return errors.New("kubernetes REST config is required for cancel")
	}
	if uid == "" || commandID == "" {
		return errors.New("pod UID and command ID are required")
	}
	if len(commandID) > 128 {
		return errors.New("command ID is too long")
	}
	pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.UID != uid {
		return errors.New("pod UID changed before cancel")
	}
	restReq := p.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(p.Namespace).Name(name).SubResource("exec")
	restReq.VersionedParams(&corev1.PodExecOptions{Container: containerName, Command: []string{agentPath, "cancel", commandID}, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(p.Config, "POST", restReq.URL())
	if err != nil {
		return safeAgentError(err)
	}
	var stdout bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: io.Discard}); err != nil {
		return safeAgentError(err)
	}
	var event supervisor.Event
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&event); err != nil {
		return safeAgentError(err)
	}
	if event.Error != "" {
		return errors.New(supervisor.SafeFailure(event.Error))
	}
	if event.ExitCode == nil {
		return errors.New("cancel response has no acknowledgement")
	}
	var extra supervisor.Event
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("cancel response has extra data")
	}
	return nil
}

// Exec streams one command through pods/exec to the in-container supervisor.
// A terminal event is required even if Kubernetes reports a clean stream EOF.
// The callback must return promptly and must not retain Event.Data.
func (p *Provider) Exec(ctx context.Context, name string, uid types.UID, req supervisor.Request, onEvent func(supervisor.Event) error) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Config == nil {
		return errors.New("kubernetes REST config is required for exec")
	}
	if uid == "" {
		return errors.New("pod UID is required for exec")
	}
	if onEvent == nil || req.Op != "run" {
		return errors.New("invalid exec callback or operation")
	}
	pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.UID != uid {
		return errors.New("pod UID changed before exec")
	}
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("pod is not running: %s", pod.Status.Phase)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(payload) > 1<<20 {
		return errors.New("command request exceeds 1 MiB")
	}
	payload = append(payload, '\n')
	restReq := p.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(p.Namespace).Name(name).SubResource("exec")
	restReq.VersionedParams(&corev1.PodExecOptions{Container: containerName, Command: []string{agentPath, "invoke"}, Stdin: true, Stdout: true, Stderr: true, TTY: false}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(p.Config, "POST", restReq.URL())
	if err != nil {
		return safeAgentError(err)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	outputReader, outputWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := executor.StreamWithContext(streamCtx, remotecommand.StreamOptions{Stdin: bytes.NewReader(payload), Stdout: outputWriter, Stderr: io.Discard, Tty: false})
		_ = outputWriter.CloseWithError(err)
		done <- err
	}()
	defer func() { _ = outputReader.Close() }()
	decoder := json.NewDecoder(outputReader)
	terminal := false
	var terminalError string
	for {
		var event supervisor.Event
		err := decoder.Decode(&event)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				cancel()
				_ = outputReader.CloseWithError(err)
				<-done
				return safeAgentError(err)
			}
			break
		}
		if event.Error != "" {
			event.Error = supervisor.SafeFailure(event.Error)
		}
		if err := onEvent(event); err != nil {
			cancel()
			_ = outputReader.CloseWithError(err)
			<-done
			return err
		}
		if event.ExitCode != nil || event.Error != "" {
			terminal = true
			terminalError = event.Error
			break
		}
	}
	// Drain before waiting for the transport goroutine. Reading only one byte
	// would leave a large trailing frame blocked in the pipe indefinitely.
	// Permit JSON trailing whitespace without retaining untrusted output.
	unexpectedOutput, readErr := drainExecOutput(decoder, outputReader)
	transportErr := <-done
	if unexpectedOutput {
		return errors.New("agent emitted output after terminal event")
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && transportErr == nil {
		return safeAgentError(readErr)
	}
	if !terminal {
		if transportErr != nil {
			return safeAgentError(transportErr)
		}
		return errors.New("agent stream ended without terminal event")
	}
	if terminalError != "" {
		return errors.New(supervisor.SafeFailure(terminalError))
	}
	if transportErr != nil {
		return safeAgentError(transportErr)
	}
	return nil
}

type execTrailer struct{ unexpected bool }

func (t *execTrailer) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
			t.unexpected = true
		}
	}
	return len(p), nil
}

func drainExecOutput(decoder *json.Decoder, output io.Reader) (bool, error) {
	var trailer execTrailer
	_, err := io.Copy(&trailer, io.MultiReader(decoder.Buffered(), output))
	return trailer.unexpected, err
}
