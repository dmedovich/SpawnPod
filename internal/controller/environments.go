package controller

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	"k8s.io/client-go/kubernetes"
)

// ListEnvironments emits durable, non-secret environment records as JSON.
func ListEnvironments(ctx context.Context, client kubernetes.Interface, namespace string, out io.Writer) error {
	store := &provider.ConfigMapStore{Client: client, Namespace: namespace}
	records, err := store.List(ctx)
	if err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return json.NewEncoder(out).Encode(records)
}

// InspectEnvironment returns one durable record. The ID is validated by the
// store before it is used as a Kubernetes resource name.
func InspectEnvironment(ctx context.Context, client kubernetes.Interface, namespace, id string, out io.Writer) error {
	store := &provider.ConfigMapStore{Client: client, Namespace: namespace}
	record, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(record)
}

type CleanupCandidate struct {
	ID     string                 `json:"id"`
	State  engine.State           `json:"state"`
	Action engine.ReconcileAction `json:"action"`
	Reason string                 `json:"reason"`
}

// PreviewCleanup computes the same record-level decisions as reconciliation.
// It cannot see in-flight operations held by a running controller.
func PreviewCleanup(ctx context.Context, client kubernetes.Interface, namespace string, now time.Time, out io.Writer) error {
	store := &provider.ConfigMapStore{Client: client, Namespace: namespace}
	records, err := store.List(ctx)
	if err != nil {
		return err
	}
	candidates := make([]CleanupCandidate, 0)
	for _, record := range records {
		action, reason := engine.PlanReconciliation(record, now)
		if action != engine.ReconcileRetain {
			candidates = append(candidates, CleanupCandidate{ID: record.ID, State: record.State, Action: action, Reason: reason})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return json.NewEncoder(out).Encode(struct {
		Note       string             `json:"note"`
		Candidates []CleanupCandidate `json:"candidates"`
	}{Note: "Preview only; active controller operations may suppress a listed action.", Candidates: candidates})
}
