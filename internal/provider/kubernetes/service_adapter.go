package kubernetes

import (
	"context"
	"errors"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	"k8s.io/apimachinery/pkg/types"
)

// EngineServices adapts service resource operations to the engine's
// platform-neutral contract.
type EngineServices struct{ Provider *Provider }

func (a *EngineServices) provider() (*Provider, error) {
	if a == nil || a.Provider == nil {
		return nil, errors.New("service provider is not configured")
	}
	return a.Provider, nil
}

func (a *EngineServices) CreateService(ctx context.Context, spec engine.ServiceSpec) (engine.ServiceHandle, error) {
	p, err := a.provider()
	if err != nil {
		return engine.ServiceHandle{}, err
	}
	handle, err := p.CreateServiceResources(ctx, spec)
	return engine.ServiceHandle{Name: handle.Name, PodUID: string(handle.PodUID), ServiceUID: string(handle.ServiceUID), EgressPolicyUID: string(handle.EgressPolicyUID), IngressPolicyUID: string(handle.IngressPolicyUID)}, err
}

func (a *EngineServices) ResolveServiceAddress(ctx context.Context, spec engine.ServiceSpec, handle engine.ServiceHandle) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	return p.ResolveServiceAddress(ctx, spec, ServiceHandle{Name: handle.Name, PodUID: types.UID(handle.PodUID), ServiceUID: types.UID(handle.ServiceUID)})
}

func (a *EngineServices) StartService(ctx context.Context, spec engine.ServiceSpec, handle engine.ServiceHandle, environment map[string]string, lifetime time.Duration) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.StartService(ctx, spec, ServiceHandle{Name: handle.Name, PodUID: types.UID(handle.PodUID), ServiceUID: types.UID(handle.ServiceUID)}, environment, lifetime)
}

func (a *EngineServices) WaitServiceReady(ctx context.Context, spec engine.ServiceSpec, handle engine.ServiceHandle) (string, error) {
	p, err := a.provider()
	if err != nil {
		return "", err
	}
	return p.WaitServiceReady(ctx, spec, ServiceHandle{Name: handle.Name, PodUID: types.UID(handle.PodUID), ServiceUID: types.UID(handle.ServiceUID)})
}

func (a *EngineServices) ServiceAlive(ctx context.Context, spec engine.ServiceSpec, handle engine.ServiceHandle) (bool, error) {
	p, err := a.provider()
	if err != nil {
		return false, err
	}
	return p.ServiceAlive(ctx, spec, ServiceHandle{Name: handle.Name, PodUID: types.UID(handle.PodUID), ServiceUID: types.UID(handle.ServiceUID)})
}

func (a *EngineServices) RemoveService(ctx context.Context, spec engine.ServiceSpec, handle engine.ServiceHandle) error {
	p, err := a.provider()
	if err != nil {
		return err
	}
	return p.RemoveServiceResources(ctx, spec.EnvironmentID, spec.Name, ServiceHandle{Name: handle.Name, PodUID: types.UID(handle.PodUID), ServiceUID: types.UID(handle.ServiceUID), EgressPolicyUID: types.UID(handle.EgressPolicyUID), IngressPolicyUID: types.UID(handle.IngressPolicyUID)})
}
