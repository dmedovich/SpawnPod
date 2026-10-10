package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	"github.com/dmedovich/spawnpod/internal/supervisor"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func serviceAcknowledged(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var event supervisor.Event
	if err := decoder.Decode(&event); err != nil {
		return fmt.Errorf("decode service agent response: %w", err)
	}
	if event.Error != "" {
		return errors.New("service agent rejected request")
	}
	if event.ExitCode == nil || *event.ExitCode != 0 || event.Stream != "" {
		return errors.New("service agent did not acknowledge request")
	}
	var extra supervisor.Event
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("service agent sent extra output")
	}
	return nil
}

func (p *Provider) waitServiceAgent(ctx context.Context, spec ServiceSpec, handle ServiceHandle) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, err := p.Client.CoreV1().Pods(p.Namespace).Get(ctx, handle.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.UID != handle.PodUID || checkServiceIdentity(pod.Name, spec.EnvironmentID, spec.Name, pod.Labels) != nil {
			return errors.New("service Pod identity changed before startup")
		}
		if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			return fmt.Errorf("service Pod finished before startup: %s", pod.Status.Phase)
		}
		if pod.Status.Phase == corev1.PodRunning {
			var output metadataBuffer
			err := p.transferContainer(ctx, engine.JobHandle{Name: handle.Name, UID: string(handle.PodUID)}, "service", []string{agentPath, "service-ping"}, nil, &output)
			if err == nil {
				if err := serviceAcknowledged(output.Bytes()); err == nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// StartService sends only the approved command and transient workflow env over
// pods/exec stdin. The agent retains the child after this exec stream closes.
func (p *Provider) StartService(ctx context.Context, spec ServiceSpec, handle ServiceHandle, environment map[string]string, lifetime time.Duration) error {
	if err := p.validate(); err != nil {
		return err
	}
	if _, err := validateServiceSpec(spec); err != nil {
		return err
	}
	if handle.Name != serviceResourceName(spec) || handle.PodUID == "" || handle.ServiceUID == "" || lifetime <= 0 || lifetime > 24*time.Hour {
		return errors.New("invalid service startup identity or lifetime")
	}
	request := supervisor.Request{Op: "start_service", ID: "service", Argv: spec.Command, Env: environment, TimeoutMillis: lifetime.Milliseconds()}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) >= 1<<20 {
		return errors.New("service startup request exceeds agent limit")
	}
	payload = append(payload, '\n')
	if err := p.waitServiceAgent(ctx, spec, handle); err != nil {
		return err
	}
	var output metadataBuffer
	if err := p.transferContainer(ctx, engine.JobHandle{Name: handle.Name, UID: string(handle.PodUID)}, "service", []string{agentPath, "service-start"}, bytes.NewReader(payload), &output); err != nil {
		return err
	}
	return serviceAcknowledged(output.Bytes())
}
