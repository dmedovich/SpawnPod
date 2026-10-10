package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestTransportErrorsDoNotLeakIntoControllerLogs(t *testing.T) {
	const canary = "audit-credential-canary"
	store := &memoryStore{}
	id := strings.Repeat("a", 32)
	_, err := store.Create(context.Background(), Record{Version: 1, ID: id, State: StateReady, Deadline: time.Now().Add(time.Hour), Job: JobHandle{Name: "sp-" + id, UID: "uid-1"}})
	if err != nil {
		t.Fatal(err)
	}
	jobs := &fakeJobs{copyInErr: errors.New("agent pods/exec: fake failure: " + canary)}
	backend := &Engine{Store: store, Jobs: jobs, CleanupTimeout: time.Second}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	if err := backend.CopyIn(context.Background(), id, "/workspace", strings.NewReader("archive")); err == nil {
		t.Fatal("expected transfer failure")
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatal("untrusted transfer stderr in an error reaches controller logs")
	}
}

type partialStore struct {
	*memoryStore
	listErr error
}

func (s *partialStore) List(ctx context.Context) ([]Record, error) {
	records, err := s.memoryStore.List(ctx)
	if err != nil {
		return nil, err
	}
	return records, s.listErr
}

func TestPartialListCleansSupportedRecordsAndBlocksCreation(t *testing.T) {
	e, store, jobs := newEngine()
	id := strings.Repeat("b", 32)
	_, err := store.Create(context.Background(), Record{Version: 1, ID: id, State: StateTerminating, Deadline: time.Now().Add(time.Hour), Job: JobHandle{Name: "sp-" + id, UID: "uid-1"}})
	if err != nil {
		t.Fatal(err)
	}
	partial := &partialStore{memoryStore: store, listErr: &PartialListError{Rejected: 1}}
	e.Store = partial
	err = e.ReconcileOnce(context.Background())
	var rejected *PartialListError
	if !errors.As(err, &rejected) || rejected.Rejected != 1 {
		t.Fatalf("partial error lost: %v", err)
	}
	record, err := store.Get(context.Background(), id)
	if err != nil || record.State != StateCleaned || jobs.removed != 1 {
		t.Fatalf("supported record not cleaned: %+v err=%v removed=%d", record, err, jobs.removed)
	}
	if _, err := e.Create(context.Background(), serviceCreateRequest()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("creation allowed with incompatible records: %v", err)
	}
	partial.listErr = nil
	if err := e.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	blocked := e.incompatible
	e.mu.Unlock()
	if blocked {
		t.Fatal("compatibility gate did not recover")
	}
}

func TestOtherListErrorsDoNotAuthorizePartialCleanup(t *testing.T) {
	e, store, jobs := newEngine()
	_, err := store.Create(context.Background(), Record{Version: 1, ID: strings.Repeat("c", 32), State: StateTerminating, Deadline: time.Now().Add(time.Hour), Job: JobHandle{Name: "pod", UID: "uid-1"}})
	if err != nil {
		t.Fatal(err)
	}
	e.Store = &partialStore{memoryStore: store, listErr: errors.New("API list incomplete")}
	if err := e.ReconcileOnce(context.Background()); err == nil || jobs.removed != 0 {
		t.Fatalf("API error authorized cleanup: %v removed=%d", err, jobs.removed)
	}
}

type unavailableFailureStore struct {
	*memoryStore
	release chan struct{}
}

func (s *unavailableFailureStore) Update(ctx context.Context, record Record) (Record, error) {
	if record.State == StateFailed || record.State == StateIndeterminate {
		select {
		case <-ctx.Done():
			return Record{}, ctx.Err()
		case <-s.release:
			return Record{}, errors.New("API unavailable")
		}
	}
	return s.memoryStore.Update(ctx, record)
}

func TestFailurePersistenceIsBoundedAndReleasesOperation(t *testing.T) {
	for _, mode := range []string{"create", "copy-in"} {
		t.Run(mode, func(t *testing.T) {
			e, store, jobs := newEngine()
			e.CleanupTimeout = 20 * time.Millisecond
			blocked := &unavailableFailureStore{memoryStore: store, release: make(chan struct{})}
			defer close(blocked.release)
			e.Store = blocked
			done := make(chan error, 1)
			if mode == "create" {
				jobs.createErr = errors.New("API unavailable")
				go func() {
					_, err := e.Create(context.Background(), CreateRequest{Owner: "runner", Profile: "default", Deadline: time.Now().Add(time.Hour)})
					done <- err
				}()
			} else {
				id := strings.Repeat("d", 32)
				if _, err := store.Create(context.Background(), Record{Version: 1, ID: id, State: StateReady, Deadline: time.Now().Add(time.Hour), Job: JobHandle{Name: "sp-" + id, UID: "uid-1"}}); err != nil {
					t.Fatal(err)
				}
				jobs.copyInErr = errors.New("transfer failed")
				go func() { done <- e.CopyIn(context.Background(), id, "/workspace", strings.NewReader("archive")) }()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected operation failure")
				}
			case <-time.After(time.Second):
				t.Fatal("failure persistence held operation indefinitely")
			}
			e.mu.Lock()
			active := len(e.active)
			e.mu.Unlock()
			if active != 0 {
				t.Fatalf("active operation leaked: %d", active)
			}
			// A fresh engine can clean the original durable intent after API recovery.
			recovered := &Engine{Store: store, Jobs: jobs, CleanupTimeout: time.Second}
			if err := recovered.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			records, err := store.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if record.State != StateCleaned {
					t.Fatalf("unrecovered record: %+v", record)
				}
			}
		})
	}
}

func TestRestartRecoveryCoversAllPersistedStates(t *testing.T) {
	for _, state := range []State{StateProvisioning, StateReady, StateExecuting, StateTransferring, StateFailed, StateIndeterminate, StateTerminating, StateCleaned} {
		t.Run(string(state), func(t *testing.T) {
			e, store, jobs := newEngine()
			record := Record{Version: 1, ID: strings.Repeat("e", 32), State: state, Deadline: time.Now().Add(time.Hour), Job: JobHandle{Name: "pod", UID: "uid-1"}}
			if state == StateCleaned {
				record.CleanedAt = time.Now()
			}
			if _, err := store.Create(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if err := e.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(context.Background(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := StateCleaned
			wantRemoved := 1
			if state == StateReady {
				want = StateReady
				wantRemoved = 0
			}
			if state == StateCleaned {
				wantRemoved = 0
			}
			if current.State != want || jobs.removed != wantRemoved {
				t.Fatalf("state=%s removed=%d", current.State, jobs.removed)
			}
			if err := e.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if jobs.removed != wantRemoved {
				t.Fatal("repeated recovery repeated cleanup")
			}
		})
	}
}
