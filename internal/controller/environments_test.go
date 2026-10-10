package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dmedovich/spawnpod/internal/engine"
	provider "github.com/dmedovich/spawnpod/internal/provider/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnvironmentInspectionAndCleanupPreview(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	store := &provider.ConfigMapStore{Client: client, Namespace: "ci"}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.Create(ctx, engine.Record{Version: 1, ID: id, Owner: "job", Profile: "default", State: engine.StateFailed, Deadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ListEnvironments(ctx, client, "ci", &out); err != nil {
		t.Fatal(err)
	}
	var list []engine.Record
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("unexpected list: %v %s", err, out.String())
	}
	out.Reset()
	if err := InspectEnvironment(ctx, client, "ci", id, &out); err != nil {
		t.Fatal(err)
	}
	var item engine.Record
	if err := json.Unmarshal(out.Bytes(), &item); err != nil || item.ID != id {
		t.Fatalf("unexpected inspect: %v %s", err, out.String())
	}
	out.Reset()
	if err := PreviewCleanup(ctx, client, "ci", now, &out); err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Candidates []CleanupCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil || len(preview.Candidates) != 1 || preview.Candidates[0].Action != engine.ReconcileRemove {
		t.Fatalf("unexpected preview: %v %s", err, out.String())
	}
	if _, err := store.Get(ctx, id); err != nil {
		t.Fatalf("preview modified record: %v", err)
	}
}
