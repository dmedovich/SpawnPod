package kubernetes

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// EngineJobs adapts the Kubernetes provider to the engine's platform-neutral
// Jobs contract.
type EngineJobs struct{ Provider *Provider }

func (a *EngineJobs) provider() (*Provider, error) {
	if a == nil || a.Provider == nil {
		return nil, errors.New("job provider is not configured")
	}
	return a.Provider, nil
}

func (a *EngineJobs) CreateJob(ctx context.Context, s engine.JobSpec) (engine.JobHandle, error) {
	p, err := a.provider()
	if err != nil {
		return engine.JobHandle{}, err
	}
	pod, err := p.CreateJob(ctx, s)
	if err != nil {
		return engine.JobHandle{}, err
	}
	return engine.JobHandle{Name: pod.Name, UID: string(pod.UID)}, nil
}

func (a *EngineJobs) FindJob(ctx context.Context, name string) (engine.JobHandle, error) {
	p, err := a.provider()
	if err != nil {
		return engine.JobHandle{}, err
	}
	if err := p.validate(); err != nil {
		return engine.JobHandle{}, err
	}
	pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return engine.JobHandle{}, engine.ErrNotFound
	}
	if err != nil {
		return engine.JobHandle{}, err
	}
	id := strings.TrimPrefix(name, "sp-")
	if name != "sp-"+id || !idPattern.MatchString(id) || pod.Labels[ownerLabel] != "true" || pod.Labels[environmentLabel] != id {
		return engine.JobHandle{}, errors.New("Pod ownership mismatch")
	}
	return engine.JobHandle{Name: pod.Name, UID: string(pod.UID)}, nil
}

func (a *EngineJobs) WaitReady(ctx context.Context, handle engine.JobHandle) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	_, err = p.WaitReady(ctx, handle.Name, types.UID(handle.UID))
	return err
}

func (a *EngineJobs) Metadata(ctx context.Context, handle engine.JobHandle) (engine.RuntimeMetadata, error) {
	p, err := a.provider()
	if err != nil {
		return engine.RuntimeMetadata{}, err
	}
	return p.Metadata(ctx, handle)
}

func (a *EngineJobs) Exec(ctx context.Context, handle engine.JobHandle, req supervisor.Request, onEvent func(supervisor.Event) error) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.Exec(ctx, handle.Name, types.UID(handle.UID), req, onEvent)
}

func (a *EngineJobs) CopyIn(ctx context.Context, handle engine.JobHandle, dest string, input io.Reader) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.CopyIn(ctx, handle, dest, input)
}

func (a *EngineJobs) CopyOut(ctx context.Context, handle engine.JobHandle, src string, output io.Writer) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.CopyOut(ctx, handle, src, output)
}

func (a *EngineJobs) CancelCommand(ctx context.Context, handle engine.JobHandle, id string) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.CancelCommand(ctx, handle.Name, types.UID(handle.UID), id)
}

func (a *EngineJobs) RemoveJob(ctx context.Context, handle engine.JobHandle) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.RemoveJob(ctx, handle.Name, types.UID(handle.UID))
}
