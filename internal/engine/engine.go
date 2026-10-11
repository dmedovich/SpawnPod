package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/dmedovich/spawnpod/internal/supervisor"
)

type operation struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type removal struct {
	done chan struct{}
	err  error
}

// Engine coordinates live operations with persistent management records.
// One active Engine instance per namespace is supported.
type Engine struct {
	Store              Store
	Jobs               Jobs
	Services           Services
	CleanupTimeout     time.Duration
	TombstoneRetention time.Duration
	Logger             *slog.Logger
	mu                 sync.Mutex
	active             map[string]*operation
	removals           map[string]*removal
	incompatible       bool
}

type CreateRequest struct {
	Owner    string
	Profile  string
	Deadline time.Time
	Job      JobSpec
	Services []ServiceSpec
	// ServiceEnvironments is used only during Create and never persisted.
	ServiceEnvironments map[string]map[string]string
}

func (e *Engine) validate() error {
	if e == nil || e.Store == nil || e.Jobs == nil || e.CleanupTimeout <= 0 {
		return errors.New("engine is not configured")
	}
	return nil
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// Create persists intent before asking Kubernetes to create the Pod. Start is
// separate, matching Forgejo's Create/Start lifecycle.
func (e *Engine) Create(ctx context.Context, req CreateRequest) (Record, error) {
	if err := e.validate(); err != nil {
		return Record{}, err
	}
	e.mu.Lock()
	incompatible := e.incompatible
	e.mu.Unlock()
	if incompatible {
		return Record{}, ErrInvalidState
	}
	if req.Owner == "" || req.Profile == "" || !req.Deadline.After(time.Now()) {
		return Record{}, errors.New("owner, profile and future deadline are required")
	}
	if len(req.Services) > 8 || (len(req.Services) > 0 && e.Services == nil) {
		return Record{}, errors.New("service count exceeds limit or service provider is missing")
	}
	seen := make(map[string]bool, len(req.Services))
	for _, service := range req.Services {
		if service.Name == "" || seen[service.Name] || service.Architecture != req.Job.Architecture {
			return Record{}, errors.New("service names must be unique and architecture must match job")
		}
		seen[service.Name] = true
	}
	for name, environment := range req.ServiceEnvironments {
		if !seen[name] {
			return Record{}, errors.New("service environment has no approved service")
		}
		for key, value := range environment {
			if key == "" || len(key) > 255 || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
				return Record{}, errors.New("invalid service environment variable")
			}
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Record{}, err
	}
	id := hex.EncodeToString(raw[:])
	req.Job.EnvironmentID = id
	req.Job.ServiceAliases = nil
	serviceRecords := make([]ServiceRecord, 0, len(req.Services))
	for _, spec := range req.Services {
		spec.EnvironmentID = id
		serviceRecords = append(serviceRecords, ServiceRecord{Spec: spec, Handle: ServiceHandle{Name: "sp-" + id + "-" + spec.Name}})
	}
	opCtx, leave, err := e.enter(ctx, id)
	if err != nil {
		return Record{}, err
	}
	defer leave()
	provisionCtx, stopProvision := context.WithTimeout(opCtx, 10*time.Minute)
	defer stopProvision()
	createCtx, stop := context.WithDeadline(provisionCtx, req.Deadline)
	defer stop()
	version := 1
	if len(serviceRecords) > 0 {
		version = 2
	}
	record := Record{Version: version, ID: id, Owner: req.Owner, Profile: req.Profile, CreatedAt: time.Now().UTC(), Deadline: req.Deadline.UTC(), State: StateProvisioning, Job: JobHandle{Name: "sp-" + id}, Services: serviceRecords}
	if _, err := e.Store.Create(createCtx, record); err != nil {
		return Record{}, err
	}
	for i, service := range serviceRecords {
		handle, createErr := e.Services.CreateService(createCtx, service.Spec)
		if handle.Name != "" {
			record, err = e.change(createCtx, id, []State{StateProvisioning}, func(r *Record) { r.Services[i].Handle = handle })
			if err != nil {
				return record, fmt.Errorf("record service resources: %w", err)
			}
		}
		if createErr != nil {
			record, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "service creation failed" })
			return record, fmt.Errorf("create service %s: %w", service.Spec.Name, createErr)
		}
		ip, addressErr := e.Services.ResolveServiceAddress(createCtx, service.Spec, handle)
		address, parseErr := netip.ParseAddr(ip)
		if addressErr != nil || parseErr != nil || !address.IsGlobalUnicast() {
			record, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "service address unavailable" })
			if addressErr != nil {
				return record, fmt.Errorf("resolve service %s address: %w", service.Spec.Name, addressErr)
			}
			return record, fmt.Errorf("service %s has invalid ClusterIP", service.Spec.Name)
		}
		record, err = e.change(createCtx, id, []State{StateProvisioning}, func(r *Record) { r.Services[i].ClusterIP = address.String() })
		if err != nil {
			return record, fmt.Errorf("record service address: %w", err)
		}
		req.Job.ServiceAliases = append(req.Job.ServiceAliases, ServiceAlias{Name: service.Spec.Name, IP: address.String()})
		lifetime := time.Until(req.Deadline)
		if lifetime <= 0 {
			record, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "service startup deadline expired" })
			return record, context.DeadlineExceeded
		}
		if err := e.Services.StartService(createCtx, service.Spec, handle, req.ServiceEnvironments[service.Spec.Name], lifetime); err != nil {
			record, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "service startup failed" })
			return record, fmt.Errorf("start service %s: %w", service.Spec.Name, err)
		}
	}
	handle, err := e.Jobs.CreateJob(createCtx, req.Job)
	if err != nil {
		record, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "job creation failed" })
		return record, fmt.Errorf("create job Pod: %w", err)
	}
	record, err = e.change(createCtx, id, []State{StateProvisioning}, func(r *Record) { r.Job = handle })
	if err != nil {
		return record, fmt.Errorf("record created job Pod: %w", err)
	}
	e.logger().Info("environment.created", "environment_id", id, "pod", handle.Name)
	return record, nil
}

// Start waits for the job agent and all approved service readiness probes.
// It does not execute a step.
func (e *Engine) Start(ctx context.Context, id string) (Record, error) {
	if err := e.validate(); err != nil {
		return Record{}, err
	}
	opCtx, leave, err := e.enter(ctx, id)
	if err != nil {
		return Record{}, err
	}
	defer leave()
	record, err := e.Store.Get(opCtx, id)
	if err != nil {
		return Record{}, err
	}
	if record.State != StateProvisioning || record.Job.UID == "" {
		return record, ErrInvalidState
	}
	deadlineCtx, stop := context.WithDeadline(opCtx, record.Deadline)
	defer stop()
	if err := e.Jobs.WaitReady(deadlineCtx, record.Job); err != nil {
		_, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "job did not become ready" })
		return record, err
	}
	serviceIPs := make([]string, len(record.Services))
	for i, service := range record.Services {
		if e.Services == nil {
			return record, errors.New("service provider is missing")
		}
		ip, err := e.Services.WaitServiceReady(deadlineCtx, service.Spec, service.Handle)
		if err == nil && (ip == "" || ip != service.ClusterIP) {
			err = errors.New("service ClusterIP changed after job Pod creation")
		}
		if err != nil {
			_, _ = e.changeAfterFailure(id, []State{StateProvisioning}, func(r *Record) { r.State = StateFailed; r.LastError = "service did not become ready" })
			return record, fmt.Errorf("service %s: %w", service.Spec.Name, err)
		}
		serviceIPs[i] = ip
	}
	record, err = e.change(deadlineCtx, id, []State{StateProvisioning}, func(r *Record) {
		r.State = StateReady
		r.LastError = ""
		for i := range r.Services {
			r.Services[i].ClusterIP = serviceIPs[i]
		}
	})
	if err == nil {
		e.logger().Info("environment.ready", "environment_id", id, "pod", record.Job.Name)
	}
	return record, err
}

// Metadata reads process environment and architecture directly from the
// running agent. It is never persisted because image variables may be secret.
func (e *Engine) Metadata(ctx context.Context, id string) (RuntimeMetadata, error) {
	if err := e.validate(); err != nil {
		return RuntimeMetadata{}, err
	}
	opCtx, leave, err := e.enter(ctx, id)
	if err != nil {
		return RuntimeMetadata{}, err
	}
	defer leave()
	record, err := e.Store.Get(opCtx, id)
	if err != nil {
		return RuntimeMetadata{}, err
	}
	if record.State != StateReady {
		return RuntimeMetadata{}, ErrInvalidState
	}
	deadlineCtx, stop := context.WithDeadline(opCtx, record.Deadline)
	defer stop()
	return e.Jobs.Metadata(deadlineCtx, record.Job)
}

// Exec records an executing state before starting the command. A nonzero
// process exit is an Event outcome and returns the environment to Ready.
// Infrastructure/transport errors are indeterminate and trigger cleanup.
func (e *Engine) Exec(ctx context.Context, id string, req supervisor.Request, onEvent func(supervisor.Event) error) (Record, error) {
	if err := e.validate(); err != nil {
		return Record{}, err
	}
	if req.Op != "run" || req.ID == "" || onEvent == nil {
		return Record{}, errors.New("invalid command request")
	}
	opCtx, leave, err := e.enter(ctx, id)
	if err != nil {
		return Record{}, err
	}
	record, err := e.change(opCtx, id, []State{StateReady}, func(r *Record) { r.State = StateExecuting; r.LastCommandID = req.ID })
	if err != nil {
		leave()
		return record, err
	}
	remaining := time.Until(record.Deadline)
	if remaining <= 0 {
		leave()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.CleanupTimeout)
		defer cancel()
		_ = e.Remove(cleanupCtx, id)
		return record, context.DeadlineExceeded
	}
	if req.TimeoutMillis <= 0 || req.TimeoutMillis > remaining.Milliseconds() {
		req.TimeoutMillis = remaining.Milliseconds()
		if req.TimeoutMillis < 1 {
			req.TimeoutMillis = 1
		}
	}
	deadlineCtx, stop := context.WithDeadline(opCtx, record.Deadline)
	started := time.Now()
	e.logger().Info("execution.started", "environment_id", id)
	exitCode := -1
	err = e.Jobs.Exec(deadlineCtx, record.Job, req, func(event supervisor.Event) error {
		if event.ExitCode != nil {
			exitCode = *event.ExitCode
		}
		return onEvent(event)
	})
	stop()
	if err != nil {
		e.logger().Error("execution.failed", "environment_id", id, "failure", "backend_operation", "duration_ms", time.Since(started).Milliseconds())
		_, _ = e.changeAfterFailure(id, []State{StateExecuting}, func(r *Record) { r.State = StateIndeterminate; r.LastError = "command transport or execution failed" })
		leave()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.CleanupTimeout)
		defer cancel()
		_ = e.Remove(cleanupCtx, id)
		return record, err
	}
	record, err = e.change(opCtx, id, []State{StateExecuting}, func(r *Record) { r.State = StateReady; r.LastCommandID = "" })
	if err == nil {
		e.logger().Info("execution.finished", "environment_id", id, "exit_code", exitCode, "duration_ms", time.Since(started).Milliseconds())
	}
	leave()
	return record, err
}

// CopyIn records a mutating transfer before it starts. An interrupted or
// failed transfer makes the workspace indeterminate and removes the Pod.
func (e *Engine) CopyIn(ctx context.Context, id, dest string, input io.Reader) error {
	if err := e.validate(); err != nil {
		return err
	}
	if input == nil {
		return errors.New("CopyIn input is required")
	}
	opCtx, leave, err := e.enter(ctx, id)
	if err != nil {
		return err
	}
	record, err := e.change(opCtx, id, []State{StateReady}, func(r *Record) { r.State = StateTransferring })
	if err != nil {
		leave()
		return err
	}
	deadlineCtx, stop := context.WithDeadline(opCtx, record.Deadline)
	err = e.Jobs.CopyIn(deadlineCtx, record.Job, dest, input)
	stop()
	if err != nil {
		e.logger().Error("environment.transfer.failed", "environment_id", id, "failure", "backend_operation")
		_, _ = e.changeAfterFailure(id, []State{StateTransferring}, func(r *Record) { r.State = StateIndeterminate; r.LastError = "file transfer failed" })
		leave()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.CleanupTimeout)
		defer cancel()
		_ = e.Remove(cleanupCtx, id)
		return err
	}
	_, err = e.change(opCtx, id, []State{StateTransferring}, func(r *Record) { r.State = StateReady })
	leave()
	return err
}

// CopyOut is read-only but still occupies the environment operation slot, so
// Remove can cancel a slow archive before deleting the Pod.
func (e *Engine) CopyOut(ctx context.Context, id, src string, output io.Writer) error {
	if err := e.validate(); err != nil {
		return err
	}
	if output == nil {
		return errors.New("CopyOut output is required")
	}
	opCtx, leave, err := e.enter(ctx, id)
	if err != nil {
		return err
	}
	defer leave()
	record, err := e.Store.Get(opCtx, id)
	if err != nil {
		return err
	}
	if record.State != StateReady {
		return ErrInvalidState
	}
	deadlineCtx, stop := context.WithDeadline(opCtx, record.Deadline)
	defer stop()
	return e.Jobs.CopyOut(deadlineCtx, record.Job, src, output)
}

// Remove records termination, cancels an active operation without holding its
// operation slot, and deletes only the recorded Pod UID. It is repeatable.
func (e *Engine) Remove(ctx context.Context, id string) error {
	if err := e.validate(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.removals == nil {
		e.removals = make(map[string]*removal)
	}
	if existing := e.removals[id]; existing != nil {
		e.mu.Unlock()
		select {
		case <-existing.done:
			return existing.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	current := &removal{done: make(chan struct{})}
	e.removals[id] = current
	e.mu.Unlock()
	cleanupCtx, cancel := context.WithTimeout(ctx, e.CleanupTimeout)
	current.err = e.removeOnce(cleanupCtx, id)
	cancel()
	e.mu.Lock()
	delete(e.removals, id)
	close(current.done)
	e.mu.Unlock()
	return current.err
}

func (e *Engine) removeOnce(ctx context.Context, id string) (result error) {
	record, err := e.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if record.State == StateCleaned {
		return nil
	}
	started := time.Now()
	e.logger().Info("environment.cleanup.started", "environment_id", id, "pod", record.Job.Name, "reason", "remove requested", "previous_state", record.State)
	defer func() {
		if result != nil {
			e.logger().Error("environment.cleanup.failed", "environment_id", id, "failure", "resource_cleanup", "duration_ms", time.Since(started).Milliseconds())
		} else {
			e.logger().Info("environment.cleanup.completed", "environment_id", id, "duration_ms", time.Since(started).Milliseconds())
		}
	}()
	record, err = e.change(ctx, id, nil, func(r *Record) { r.State = StateTerminating })
	if err != nil {
		return err
	}
	e.mu.Lock()
	op := e.active[id]
	if op != nil {
		op.cancel()
	}
	e.mu.Unlock()
	if op != nil {
		if record.LastCommandID != "" && record.Job.UID != "" {
			_ = e.Jobs.CancelCommand(ctx, record.Job, record.LastCommandID)
		}
		select {
		case <-op.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	handle := record.Job
	var cleanupFailures []error
	if handle.UID == "" {
		found, findErr := e.Jobs.FindJob(ctx, handle.Name)
		if findErr == nil {
			handle = found
		} else if !errors.Is(findErr, ErrNotFound) {
			cleanupFailures = append(cleanupFailures, fmt.Errorf("find job Pod: %w", findErr))
		}
	}
	if handle.UID != "" {
		if err := e.Jobs.RemoveJob(ctx, handle); err != nil {
			cleanupFailures = append(cleanupFailures, fmt.Errorf("remove job Pod: %w", err))
		}
	}
	for i := len(record.Services) - 1; i >= 0; i-- {
		service := record.Services[i]
		if e.Services == nil {
			cleanupFailures = append(cleanupFailures, errors.New("service provider is missing"))
			break
		}
		if err := e.Services.RemoveService(ctx, service.Spec, service.Handle); err != nil {
			cleanupFailures = append(cleanupFailures, fmt.Errorf("remove service %s: %w", service.Spec.Name, err))
		}
	}
	if len(cleanupFailures) > 0 {
		return errors.Join(cleanupFailures...)
	}
	_, err = e.change(ctx, id, []State{StateTerminating}, func(r *Record) {
		r.State = StateCleaned
		r.CleanedAt = time.Now().UTC()
		r.Job = handle
		r.LastCommandID = ""
		r.LastError = ""
	})
	return err
}

func (e *Engine) enter(parent context.Context, id string) (context.Context, func(), error) {
	e.mu.Lock()
	if e.active == nil {
		e.active = make(map[string]*operation)
	}
	if e.active[id] != nil {
		e.mu.Unlock()
		return nil, nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(parent)
	op := &operation{cancel: cancel, done: make(chan struct{})}
	e.active[id] = op
	e.mu.Unlock()
	leave := func() {
		e.mu.Lock()
		if e.active[id] == op {
			delete(e.active, id)
		}
		close(op.done)
		e.mu.Unlock()
		cancel()
	}
	return ctx, leave, nil
}

func (e *Engine) change(ctx context.Context, id string, allowed []State, mutate func(*Record)) (Record, error) {
	for attempts := 0; attempts < 5; attempts++ {
		record, err := e.Store.Get(ctx, id)
		if err != nil {
			return Record{}, err
		}
		if record.State == StateCleaned {
			return record, ErrInvalidState
		}
		if len(allowed) > 0 {
			valid := false
			for _, state := range allowed {
				if record.State == state {
					valid = true
					break
				}
			}
			if !valid {
				return record, ErrInvalidState
			}
		}
		mutate(&record)
		updated, err := e.Store.Update(ctx, record)
		if errors.Is(err, ErrConflict) {
			continue
		}
		return updated, err
	}
	return Record{}, ErrConflict
}

// changeAfterFailure records best-effort failure state even after the request
// was cancelled. A failed API must not hold the operation slot indefinitely.
func (e *Engine) changeAfterFailure(id string, allowed []State, mutate func(*Record)) (Record, error) {
	ctx, cancel := context.WithTimeout(context.Background(), e.CleanupTimeout)
	defer cancel()
	return e.change(ctx, id, allowed, mutate)
}

// ErrConflict denotes a storage compare-and-swap collision.
var ErrConflict = errors.New("environment record conflict")

// ReconcileOnce cleans interrupted/expired records after restart. Live active
// operations in this process are skipped. Idle ready environments are retained.
func (e *Engine) ReconcileOnce(ctx context.Context) error {
	if err := e.validate(); err != nil {
		return err
	}
	records, err := e.Store.List(ctx)
	var failures []error
	if err != nil {
		var partial *PartialListError
		if !errors.As(err, &partial) {
			return err
		}
		failures = append(failures, err)
	}
	e.mu.Lock()
	e.incompatible = err != nil
	e.mu.Unlock()
	now := time.Now()
	for _, record := range records {
		action, reason := PlanReconciliationWithRetention(record, now, e.TombstoneRetention)
		if action == ReconcileExpireTombstone {
			if err := e.Store.Delete(ctx, record.ID, record.Revision); err != nil {
				e.logger().Error("environment.record.expiration.failed", "environment_id", record.ID, "failure", "store_delete")
				failures = append(failures, fmt.Errorf("expire tombstone %s: %w", record.ID, err))
			} else {
				e.logger().Info("environment.record.expired", "environment_id", record.ID, "reason", reason)
			}
			continue
		}
		if action == ReconcileRetain && (record.State != StateReady || len(record.Services) == 0) {
			continue
		}
		// List is a snapshot. A live operation can move an executing or
		// transferring record back to Ready before this loop reaches it.
		// Re-read and use a revision-checked transition before cleanup.
		current, err := e.Store.Get(ctx, record.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile get %s: %w", record.ID, err))
			continue
		}
		currentAction, reason := PlanReconciliationWithRetention(current, time.Now(), e.TombstoneRetention)
		checkServices := currentAction == ReconcileRetain && current.State == StateReady && len(current.Services) > 0
		if currentAction != ReconcileRemove && !checkServices {
			continue
		}
		e.mu.Lock()
		active := e.active[record.ID] != nil
		e.mu.Unlock()
		if active {
			continue
		}
		if checkServices {
			if e.Services == nil {
				failures = append(failures, fmt.Errorf("check services %s: provider is missing", record.ID))
				continue
			}
			alive := true
			for _, service := range current.Services {
				ok, err := e.Services.ServiceAlive(ctx, service.Spec, service.Handle)
				if err != nil {
					failures = append(failures, fmt.Errorf("check service %s/%s: %w", record.ID, service.Spec.Name, err))
					alive = true // An API error cannot establish terminal loss.
					break
				}
				if !ok {
					alive = false
					break
				}
			}
			if alive {
				continue
			}
			reason = "service lost"
		}
		current.State = StateTerminating
		if _, err := e.Store.Update(ctx, current); errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			// Another operation changed the decision; the next sweep can
			// reconsider its new state.
			continue
		} else if err != nil {
			failures = append(failures, fmt.Errorf("reconcile claim %s: %w", record.ID, err))
			continue
		}
		e.logger().Info("environment.reconcile.cleanup", "environment_id", record.ID, "reason", reason)
		if err := e.Remove(ctx, record.ID); err != nil {
			failures = append(failures, fmt.Errorf("reconcile %s: %w", record.ID, err))
		}
	}
	return errors.Join(failures...)
}
