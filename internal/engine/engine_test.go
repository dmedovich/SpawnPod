package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/supervisor"
)

type memoryStore struct {
	mu      sync.Mutex
	records map[string]Record
	next    int
}

type staleListStore struct {
	*memoryStore
	afterList func()
}

type lostCreateResponseStore struct{ *memoryStore }

func (s *lostCreateResponseStore) Create(ctx context.Context, record Record) (Record, error) {
	if _, err := s.memoryStore.Create(ctx, record); err != nil {
		return Record{}, err
	}
	return Record{}, errors.New("API response lost after ConfigMap creation")
}

type racedClaimStore struct {
	*memoryStore
	beforeClaim func()
}

type blockedReadyStore struct {
	*memoryStore
	entered chan struct{}
	release chan struct{}
}

func (s *blockedReadyStore) Update(ctx context.Context, record Record) (Record, error) {
	if record.State == StateReady {
		close(s.entered)
		<-s.release
	}
	return s.memoryStore.Update(ctx, record)
}

func (s *racedClaimStore) Update(ctx context.Context, record Record) (Record, error) {
	if record.State == StateTerminating && s.beforeClaim != nil {
		s.beforeClaim()
	}
	return s.memoryStore.Update(ctx, record)
}

func (s *staleListStore) List(ctx context.Context) ([]Record, error) {
	records, err := s.memoryStore.List(ctx)
	if err == nil && s.afterList != nil {
		s.afterList()
	}
	return records, err
}

func (s *memoryStore) Create(_ context.Context, r Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.records == nil {
		s.records = map[string]Record{}
	}
	if _, ok := s.records[r.ID]; ok {
		return Record{}, ErrConflict
	}
	s.next++
	r.Revision = fmt.Sprint(s.next)
	s.records[r.ID] = r
	return r, nil
}
func (s *memoryStore) Get(_ context.Context, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}
func (s *memoryStore) Update(_ context.Context, r Record) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.records[r.ID]
	if !ok {
		return Record{}, ErrNotFound
	}
	if old.Revision != r.Revision {
		return Record{}, ErrConflict
	}
	s.next++
	r.Revision = fmt.Sprint(s.next)
	s.records[r.ID] = r
	return r, nil
}
func (s *memoryStore) List(_ context.Context) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		result = append(result, r)
	}
	return result, nil
}
func (s *memoryStore) Delete(_ context.Context, id, revision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok {
		return nil
	}
	if r.Revision != revision {
		return ErrConflict
	}
	delete(s.records, id)
	return nil
}

type fakeJobs struct {
	mu            sync.Mutex
	removed       int
	cancelled     int
	blockExec     bool
	execStarted   chan struct{}
	createStarted chan struct{}
	releaseCreate chan struct{}
	copyInErr     error
	createErr     error
	findErr       error
	removeErr     error
	createdSpec   JobSpec
}

func (j *fakeJobs) CreateJob(_ context.Context, s JobSpec) (JobHandle, error) {
	j.createdSpec = s
	if j.createStarted != nil {
		close(j.createStarted)
		<-j.releaseCreate
	}
	if j.createErr != nil {
		return JobHandle{}, j.createErr
	}
	return JobHandle{Name: "sp-" + s.EnvironmentID, UID: "uid-1"}, nil
}
func (j *fakeJobs) FindJob(_ context.Context, name string) (JobHandle, error) {
	if j.findErr != nil {
		return JobHandle{}, j.findErr
	}
	return JobHandle{Name: name, UID: "uid-1"}, nil
}
func (j *fakeJobs) WaitReady(context.Context, JobHandle) error { return nil }
func (j *fakeJobs) Metadata(context.Context, JobHandle) (RuntimeMetadata, error) {
	return RuntimeMetadata{OS: "Linux", Arch: "amd64", Environment: map[string]string{"PATH": "/bin"}}, nil
}
func (j *fakeJobs) Exec(ctx context.Context, _ JobHandle, _ supervisor.Request, emit func(supervisor.Event) error) error {
	if j.blockExec {
		close(j.execStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	code := 7
	return emit(supervisor.Event{ExitCode: &code})
}
func (j *fakeJobs) CopyIn(_ context.Context, _ JobHandle, _ string, input io.Reader) error {
	_, _ = io.Copy(io.Discard, input)
	return j.copyInErr
}
func (j *fakeJobs) CopyOut(_ context.Context, _ JobHandle, _ string, output io.Writer) error {
	_, err := io.WriteString(output, "archive")
	return err
}
func (j *fakeJobs) CancelCommand(context.Context, JobHandle, string) error {
	j.mu.Lock()
	j.cancelled++
	j.mu.Unlock()
	return nil
}
func (j *fakeJobs) RemoveJob(context.Context, JobHandle) error {
	j.mu.Lock()
	j.removed++
	j.mu.Unlock()
	return j.removeErr
}

type fakeServices struct {
	created    int
	started    int
	removed    int
	createErr  error
	startErr   error
	removeErr  error
	readyIP    string
	startupEnv map[string]string
	dead       bool
	checkErr   error
}

func (s *fakeServices) CreateService(_ context.Context, spec ServiceSpec) (ServiceHandle, error) {
	s.created++
	handle := ServiceHandle{Name: "sp-" + spec.EnvironmentID + "-" + spec.Name, PodUID: "pod-uid", ServiceUID: "service-uid", EgressPolicyUID: "egress-uid", IngressPolicyUID: "ingress-uid"}
	if s.createErr != nil {
		handle.PodUID = ""
	}
	return handle, s.createErr
}
func (s *fakeServices) WaitServiceReady(_ context.Context, spec ServiceSpec, handle ServiceHandle) (string, error) {
	if handle.Name != "sp-"+spec.EnvironmentID+"-"+spec.Name || handle.PodUID != "pod-uid" || handle.ServiceUID != "service-uid" {
		return "", errors.New("wrong service handle")
	}
	if s.readyIP != "" {
		return s.readyIP, nil
	}
	return "10.43.1.9", nil
}
func (s *fakeServices) ResolveServiceAddress(context.Context, ServiceSpec, ServiceHandle) (string, error) {
	return "10.43.1.9", nil
}
func (s *fakeServices) StartService(_ context.Context, _ ServiceSpec, _ ServiceHandle, environment map[string]string, lifetime time.Duration) error {
	if lifetime <= 0 {
		return errors.New("missing service lifetime")
	}
	s.started++
	s.startupEnv = environment
	return s.startErr
}
func (s *fakeServices) ServiceAlive(context.Context, ServiceSpec, ServiceHandle) (bool, error) {
	return !s.dead, s.checkErr
}
func (s *fakeServices) RemoveService(_ context.Context, spec ServiceSpec, handle ServiceHandle) error {
	if handle.Name != "sp-"+spec.EnvironmentID+"-"+spec.Name {
		return errors.New("wrong service intent")
	}
	s.removed++
	return s.removeErr
}

func newEngine() (*Engine, *memoryStore, *fakeJobs) {
	store := &memoryStore{}
	jobs := &fakeJobs{}
	return &Engine{Store: store, Jobs: jobs, CleanupTimeout: time.Second}, store, jobs
}

func serviceCreateRequest() CreateRequest {
	return CreateRequest{
		Owner: "runner-a", Profile: "default", Deadline: time.Now().Add(time.Hour),
		Job: JobSpec{Image: "approved", Architecture: "amd64"}, Services: []ServiceSpec{{Name: "redis", Image: "approved-service", Architecture: "amd64", Port: 6379}},
		ServiceEnvironments: map[string]map[string]string{"redis": {"SERVICE_TOKEN": "private-value"}},
	}
}

func TestServiceLifecyclePersistsIntentAndUIDs(t *testing.T) {
	e, store, jobs := newEngine()
	services := &fakeServices{}
	e.Services = services
	record, err := e.Create(context.Background(), serviceCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if record.Version != 2 || len(record.Services) != 1 || record.Services[0].Spec.EnvironmentID != record.ID || record.Services[0].Handle.PodUID != "pod-uid" || record.Services[0].Handle.EgressPolicyUID != "egress-uid" || record.Services[0].Handle.IngressPolicyUID != "ingress-uid" {
		t.Fatalf("service intent or UIDs missing: %+v", record.Services)
	}
	if len(jobs.createdSpec.ServiceAliases) != 1 || jobs.createdSpec.ServiceAliases[0] != (ServiceAlias{Name: "redis", IP: "10.43.1.9"}) || record.Services[0].ClusterIP != "10.43.1.9" {
		t.Fatalf("job alias was not derived from ClusterIP: %+v", jobs.createdSpec.ServiceAliases)
	}
	encoded, err := json.Marshal(record)
	if err != nil || strings.Contains(string(encoded), "private-value") || services.started != 1 || services.startupEnv["SERVICE_TOKEN"] != "private-value" {
		t.Fatalf("service startup or secret persistence failed: started=%d err=%v", services.started, err)
	}
	record, err = e.Start(context.Background(), record.ID)
	if err != nil || record.State != StateReady || record.Services[0].ClusterIP != "10.43.1.9" {
		t.Fatalf("service readiness not recorded: %+v, %v", record, err)
	}
	if err := e.Remove(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateCleaned || services.created != 1 || services.removed != 1 {
		t.Fatalf("cleanup record=%+v created=%d removed=%d err=%v", stored, services.created, services.removed, err)
	}
}

func TestReconcileRemovesReadyEnvironmentAfterServiceDies(t *testing.T) {
	e, store, _ := newEngine()
	services := &fakeServices{}
	e.Services = services
	record, err := e.Create(context.Background(), serviceCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if current, err := store.Get(context.Background(), record.ID); err != nil || current.State != StateReady {
		t.Fatalf("healthy service was removed: state=%s err=%v", current.State, err)
	}
	services.checkErr = errors.New("temporary API outage")
	if err := e.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("service health API error was hidden")
	}
	if current, err := store.Get(context.Background(), record.ID); err != nil || current.State != StateReady {
		t.Fatalf("API error removed service: state=%s err=%v", current.State, err)
	}
	services.checkErr = nil
	services.dead = true
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if current, err := store.Get(context.Background(), record.ID); err != nil || current.State != StateCleaned || services.removed != 1 {
		t.Fatalf("dead service was not removed: state=%s removed=%d err=%v", current.State, services.removed, err)
	}
}

func TestPartialServiceCreateIsRecoveredByReconcile(t *testing.T) {
	e, store, _ := newEngine()
	services := &fakeServices{createErr: errors.New("Pod admission rejected")}
	e.Services = services
	record, err := e.Create(context.Background(), serviceCreateRequest())
	if err == nil || record.State != StateFailed || len(record.Services) != 1 || record.Services[0].Handle.ServiceUID != "service-uid" {
		t.Fatalf("partial create record=%+v err=%v", record, err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateCleaned || services.removed != 1 {
		t.Fatalf("partial cleanup record=%+v removed=%d err=%v", stored, services.removed, err)
	}
}

func TestLostJobCreateResponseIsRecoveredByReconcile(t *testing.T) {
	e, store, jobs := newEngine()
	jobs.createErr = errors.New("API response lost after Pod creation")
	record, err := e.Create(context.Background(), CreateRequest{
		Owner: "runner-a", Profile: "default", Deadline: time.Now().Add(time.Hour),
		Job: JobSpec{Image: "approved", Architecture: "amd64"},
	})
	if err == nil || record.State != StateFailed || record.Job.UID != "" {
		t.Fatalf("ambiguous Create was not recorded as failed: record=%+v err=%v", record, err)
	}
	// FindJob represents the Pod created by the API before its response was lost.
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateCleaned || stored.Job.UID != "uid-1" || jobs.removed != 1 {
		t.Fatalf("ambiguous job Pod was not recovered: record=%+v removed=%d err=%v", stored, jobs.removed, err)
	}
}

func TestLostRecordCreateResponseIsRecoveredByReconcile(t *testing.T) {
	e, store, jobs := newEngine()
	e.Store = &lostCreateResponseStore{store}
	jobs.findErr = ErrNotFound
	if _, err := e.Create(context.Background(), CreateRequest{
		Owner: "runner-a", Profile: "default", Deadline: time.Now().Add(time.Hour),
		Job: JobSpec{Image: "approved", Architecture: "amd64"},
	}); err == nil {
		t.Fatal("lost ConfigMap response was not reported")
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, err := store.List(context.Background())
	if err != nil || len(records) != 1 || records[0].State != StateCleaned || jobs.removed != 0 {
		t.Fatalf("durable intent was not recovered: records=%+v removed=%d err=%v", records, jobs.removed, err)
	}
}

func TestServiceCleanupContinuesAfterJobFailure(t *testing.T) {
	e, store, jobs := newEngine()
	services := &fakeServices{}
	e.Services = services
	record, err := e.Create(context.Background(), serviceCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	jobs.removeErr = errors.New("job API failure")
	if err := e.Remove(context.Background(), record.ID); err == nil || services.removed != 1 {
		t.Fatalf("cleanup error=%v services removed=%d", err, services.removed)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateTerminating {
		t.Fatalf("cleanup prematurely completed: %+v, %v", stored, err)
	}
	jobs.removeErr = nil
	if err := e.Remove(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	stored, err = store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateCleaned {
		t.Fatalf("cleanup did not recover: %+v, %v", stored, err)
	}
}

func TestServiceAddressChangeFailsStart(t *testing.T) {
	e, store, _ := newEngine()
	services := &fakeServices{readyIP: "10.43.1.10"}
	e.Services = services
	record, err := e.Create(context.Background(), serviceCreateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(context.Background(), record.ID); err == nil {
		t.Fatal("Start accepted a changed service ClusterIP")
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateFailed {
		t.Fatalf("changed address record=%+v err=%v", stored, err)
	}
}

func TestServiceStartupFailureReconcilesResources(t *testing.T) {
	e, store, _ := newEngine()
	services := &fakeServices{startErr: errors.New("service agent unavailable")}
	e.Services = services
	record, err := e.Create(context.Background(), serviceCreateRequest())
	if err == nil || record.State != StateFailed {
		t.Fatalf("service startup should fail Create: state=%s err=%v", record.State, err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateCleaned || services.removed != 1 {
		t.Fatalf("failed startup cleanup: state=%s removed=%d err=%v", stored.State, services.removed, err)
	}
}

func TestCreateIgnoresCallerSuppliedJobAliases(t *testing.T) {
	e, _, jobs := newEngine()
	request := CreateRequest{
		Owner: "runner-a", Profile: "default", Deadline: time.Now().Add(time.Hour),
		Job: JobSpec{Image: "approved", ServiceAliases: []ServiceAlias{{Name: "metadata", IP: "169.254.169.254"}}},
	}
	if _, err := e.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(jobs.createdSpec.ServiceAliases) != 0 {
		t.Fatalf("caller aliases reached job Pod: %+v", jobs.createdSpec.ServiceAliases)
	}
}

func TestCopyInFailureCleansEnvironment(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	jobs.copyInErr = errors.New("broken upload")
	if err := e.CopyIn(context.Background(), record.ID, "/workspace", strings.NewReader("data")); err == nil {
		t.Fatal("expected copy failure")
	}
	result, err := store.Get(context.Background(), record.ID)
	if err != nil || result.State != StateCleaned {
		t.Fatalf("record=%+v err=%v", result, err)
	}
	if jobs.removed != 1 {
		t.Fatalf("removed %d Pods", jobs.removed)
	}
}

func TestCopyOutLeavesEnvironmentReady(t *testing.T) {
	e, store, _ := newEngine()
	record := createReady(t, e)
	var output strings.Builder
	if err := e.CopyOut(context.Background(), record.ID, "/workspace/file", &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "archive" {
		t.Fatalf("output=%q", output.String())
	}
	result, err := store.Get(context.Background(), record.ID)
	if err != nil || result.State != StateReady {
		t.Fatalf("record=%+v err=%v", result, err)
	}
}

func TestMetadataIsReadFromReadyPod(t *testing.T) {
	e, _, _ := newEngine()
	record := createReady(t, e)
	metadata, err := e.Metadata(context.Background(), record.ID)
	if err != nil || metadata.Arch != "amd64" || metadata.Environment["PATH"] != "/bin" {
		t.Fatalf("metadata=%+v err=%v", metadata, err)
	}
}

func TestReconcileInterruptedCopyIn(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	record.State = StateTransferring
	if _, err := store.Update(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := store.Get(context.Background(), record.ID)
	if err != nil || result.State != StateCleaned || jobs.removed != 1 {
		t.Fatalf("record=%+v removed=%d err=%v", result, jobs.removed, err)
	}
}

func TestReconcileSkipsCopyInUntilReadyIsRecorded(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	blocked := &blockedReadyStore{memoryStore: store, entered: make(chan struct{}), release: make(chan struct{})}
	e.Store = blocked
	copyDone := make(chan error, 1)
	go func() { copyDone <- e.CopyIn(context.Background(), record.ID, "/workspace", strings.NewReader("data")) }()
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("CopyIn did not reach Ready update")
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(blocked.release)
	if err := <-copyDone; err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateReady || jobs.removed != 0 {
		t.Fatalf("active copy was removed: state=%s removals=%d err=%v", stored.State, jobs.removed, err)
	}
}
func createReady(t *testing.T, e *Engine) Record {
	t.Helper()
	record, err := e.Create(context.Background(), CreateRequest{Owner: "runner-a", Profile: "default", Deadline: time.Now().Add(time.Hour), Job: JobSpec{Image: "approved"}})
	if err != nil {
		t.Fatal(err)
	}
	record, err = e.Start(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestLifecycleAndNonzeroCommandExit(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	code := 0
	result, err := e.Exec(context.Background(), record.ID, supervisor.Request{Op: "run", ID: "step-1"}, func(ev supervisor.Event) error {
		if ev.ExitCode != nil {
			code = *ev.ExitCode
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || result.State != StateReady {
		t.Fatalf("exit=%d state=%s", code, result.State)
	}
	if err := e.Remove(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Get(context.Background(), record.ID)
	if stored.State != StateCleaned || jobs.removed != 1 {
		t.Fatalf("state=%s removals=%d", stored.State, jobs.removed)
	}
}

func TestRemoveCancelsActiveExec(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	jobs.blockExec = true
	jobs.execStarted = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.Exec(context.Background(), record.ID, supervisor.Request{Op: "run", ID: "long"}, func(supervisor.Event) error { return nil })
		done <- err
	}()
	select {
	case <-jobs.execStarted:
	case <-time.After(time.Second):
		t.Fatal("exec did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.Remove(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exec error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("exec did not finish")
	}
	stored, _ := store.Get(context.Background(), record.ID)
	if stored.State != StateCleaned || jobs.removed != 1 || jobs.cancelled != 1 {
		t.Fatalf("state=%s removals=%d cancels=%d", stored.State, jobs.removed, jobs.cancelled)
	}
}

func TestReconcileInterruptedExecution(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	record.State = StateExecuting
	record.LastCommandID = "lost"
	if _, err := store.Update(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Get(context.Background(), record.ID)
	if stored.State != StateCleaned || jobs.removed != 1 {
		t.Fatalf("state=%s removals=%d", stored.State, jobs.removed)
	}
}

func TestReconcileDoesNotRemoveCompletedOperationFromStaleList(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	record.State = StateTransferring
	if _, err := store.Update(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	e.Store = &staleListStore{memoryStore: store, afterList: func() {
		current, err := store.Get(context.Background(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		current.State = StateReady
		if _, err := store.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateReady || jobs.removed != 0 {
		t.Fatalf("completed operation was removed: state=%s removals=%d err=%v", stored.State, jobs.removed, err)
	}
}

func TestReconcileDoesNotRemoveOperationCompletedBeforeClaim(t *testing.T) {
	e, store, jobs := newEngine()
	record := createReady(t, e)
	record.State = StateExecuting
	if _, err := store.Update(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	e.Store = &racedClaimStore{memoryStore: store, beforeClaim: func() {
		current, err := store.Get(context.Background(), record.ID)
		if err != nil {
			t.Fatal(err)
		}
		current.State = StateReady
		if _, err := store.Update(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), record.ID)
	if err != nil || stored.State != StateReady || jobs.removed != 0 {
		t.Fatalf("completed operation was removed: state=%s removals=%d err=%v", stored.State, jobs.removed, err)
	}
}

func TestRemoveDuringCreateFindsLatePod(t *testing.T) {
	e, store, jobs := newEngine()
	jobs.createStarted = make(chan struct{})
	jobs.releaseCreate = make(chan struct{})
	createDone := make(chan error, 1)
	go func() {
		_, err := e.Create(context.Background(), CreateRequest{Owner: "runner-a", Profile: "default", Deadline: time.Now().Add(time.Hour), Job: JobSpec{Image: "approved"}})
		createDone <- err
	}()
	select {
	case <-jobs.createStarted:
	case <-time.After(time.Second):
		t.Fatal("create did not reach provider")
	}
	records, _ := store.List(context.Background())
	if len(records) != 1 {
		t.Fatalf("records=%d", len(records))
	}
	id := records[0].ID
	removeDone := make(chan error, 1)
	go func() { removeDone <- e.Remove(context.Background(), id) }()
	deadline := time.Now().Add(time.Second)
	for {
		r, _ := store.Get(context.Background(), id)
		if r.State == StateTerminating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("remove did not mark termination")
		}
		time.Sleep(time.Millisecond)
	}
	close(jobs.releaseCreate)
	if err := <-createDone; err == nil {
		t.Fatal("cancelled create reported success")
	}
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Get(context.Background(), id)
	if stored.State != StateCleaned || jobs.removed != 1 {
		t.Fatalf("state=%s removals=%d", stored.State, jobs.removed)
	}
}

func TestReconcileExpiresCleanedTombstone(t *testing.T) {
	e, store, _ := newEngine()
	record := createReady(t, e)
	if err := e.Remove(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ := store.Get(context.Background(), record.ID)
	stored.CleanedAt = time.Now().Add(-25 * time.Hour)
	if _, err := store.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), record.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tombstone still exists: %v", err)
	}
}

type failingDeleteStore struct {
	*memoryStore
	err error
}

func (s *failingDeleteStore) Delete(ctx context.Context, id, revision string) error {
	if s.err != nil {
		return s.err
	}
	return s.memoryStore.Delete(ctx, id, revision)
}

func TestRetentionRepeatedJobsRestartAndDeleteRetry(t *testing.T) {
	e, store, jobs := newEngine()
	e.TombstoneRetention = 5 * time.Minute
	var logs bytes.Buffer
	e.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	for i := 0; i < 3; i++ {
		record := createReady(t, e)
		if _, err := e.Exec(context.Background(), record.ID, supervisor.Request{Op: "run", ID: "private-command", Argv: []string{"private-script"}, Env: map[string]string{"TOKEN": "private-token"}}, func(supervisor.Event) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := e.Remove(context.Background(), record.ID); err != nil {
			t.Fatal(err)
		}
		if err := e.Remove(context.Background(), record.ID); err != nil {
			t.Fatal(err)
		}
	}
	if jobs.removed != 3 {
		t.Fatalf("non-idempotent removal: %d", jobs.removed)
	}
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	records, _ := store.List(context.Background())
	if len(records) != 3 {
		t.Fatal("recent tombstones were deleted")
	}
	for _, record := range records {
		if record.State != StateCleaned || record.CleanedAt.IsZero() || record.LastCommandID != "" {
			t.Fatalf("invalid cleaned record: %+v", record)
		}
		record.CleanedAt = time.Now().Add(-6 * time.Minute)
		if _, err := store.Update(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	failing := &failingDeleteStore{memoryStore: store, err: errors.New("temporary delete failure")}
	restarted := &Engine{Store: failing, Jobs: jobs, CleanupTimeout: time.Second, TombstoneRetention: 5 * time.Minute, Logger: e.Logger}
	if err := restarted.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("lost delete error")
	}
	if records, _ := store.List(context.Background()); len(records) != 3 {
		t.Fatal("failed delete lost records")
	}
	failing.err = nil
	if err := restarted.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if records, _ := store.List(context.Background()); len(records) != 0 {
		t.Fatal("expired records accumulated")
	}
	for _, event := range []string{"environment.created", "environment.ready", "execution.started", "execution.finished", "environment.cleanup.started", "environment.cleanup.completed", "environment.record.expiration.failed", "environment.record.expired", "\"exit_code\":7", "environment_id"} {
		if !strings.Contains(logs.String(), event) {
			t.Fatalf("missing lifecycle event %s", event)
		}
	}
	for _, secret := range []string{"private-script", "private-token", "private-command"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("request content leaked into logs")
		}
	}
}
