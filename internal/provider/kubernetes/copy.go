package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/workspace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type metadataBuffer struct{ bytes.Buffer }

func (b *metadataBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("agent metadata exceeds 1 MiB")
	}
	return b.Buffer.Write(p)
}

func (p *Provider) transfer(ctx context.Context, handle engine.JobHandle, command []string, stdin io.Reader, stdout io.Writer) error {
	return p.transferContainer(ctx, handle, containerName, command, stdin, stdout)
}

func (p *Provider) transferContainer(ctx context.Context, handle engine.JobHandle, targetContainer string, command []string, stdin io.Reader, stdout io.Writer) error {
	if err := p.validate(); err != nil {
		return err
	}
	if p.Config == nil || handle.UID == "" {
		return errors.New("Kubernetes REST config and Pod UID are required")
	}
	pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if pod.UID != types.UID(handle.UID) {
		return errors.New("Pod UID changed before file transfer")
	}
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("Pod is not running: %s", pod.Status.Phase)
	}
	restReq := p.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(p.Namespace).Name(handle.Name).SubResource("exec")
	restReq.VersionedParams(&corev1.PodExecOptions{Container: targetContainer, Command: command, Stdin: stdin != nil, Stdout: true, Stderr: true, TTY: false}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(p.Config, "POST", restReq.URL())
	if err != nil {
		return safeAgentError(err)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: io.Discard, Tty: false}); err != nil {
		return safeAgentError(err)
	}
	return nil
}

func (p *Provider) CopyIn(ctx context.Context, handle engine.JobHandle, dest string, input io.Reader) error {
	rel, err := workspace.Relative(dest)
	if err != nil {
		return err
	}
	if input == nil {
		return errors.New("CopyIn input is required")
	}
	return p.transfer(ctx, handle, []string{agentPath, "copy-in", path.Join(workspace.Mount, rel)}, input, nil)
}

func (p *Provider) CopyOut(ctx context.Context, handle engine.JobHandle, src string, output io.Writer) error {
	rel, err := workspace.Relative(src)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("CopyOut output is required")
	}
	return p.transfer(ctx, handle, []string{agentPath, "copy-out", path.Join(workspace.Mount, rel)}, nil, output)
}

func (p *Provider) Metadata(ctx context.Context, handle engine.JobHandle) (engine.RuntimeMetadata, error) {
	var output metadataBuffer
	if err := p.transfer(ctx, handle, []string{agentPath, "metadata"}, nil, &output); err != nil {
		return engine.RuntimeMetadata{}, err
	}
	var metadata engine.RuntimeMetadata
	if err := json.Unmarshal(output.Bytes(), &metadata); err != nil {
		return engine.RuntimeMetadata{}, fmt.Errorf("decode agent metadata: %w", err)
	}
	if metadata.OS != "Linux" || (metadata.Arch != "amd64" && metadata.Arch != "arm64") {
		return engine.RuntimeMetadata{}, errors.New("unsupported agent OS or architecture")
	}
	return metadata, nil
}

// Do not wrap transport errors: they can contain request values or remote stderr.
func safeAgentError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New("agent operation failed")
}
