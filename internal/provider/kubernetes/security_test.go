package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestMixedRecordsPreserveUnsupportedAndListSupported(t *testing.T) {
	for _, bad := range []string{`{"version":3}`, `{"invalid":`, `{"version":1,"unknown":"credential-canary"}`} {
		supported, err := recordMap(engine.Record{Version: 1, ID: strings.Repeat("a", 32), State: engine.StateTerminating})
		if err != nil {
			t.Fatal(err)
		}
		unknown, err := recordMap(engine.Record{Version: 2, ID: strings.Repeat("b", 32)})
		if err != nil {
			t.Fatal(err)
		}
		supported.Namespace = "ci"
		unknown.Namespace = "ci"
		unknown.Data[recordKey] = bad
		store := &ConfigMapStore{Namespace: "ci", Client: fake.NewClientset(supported, unknown)}
		records, err := store.List(context.Background())
		var partial *engine.PartialListError
		if len(records) != 1 || records[0].ID != strings.Repeat("a", 32) || !errors.As(err, &partial) || partial.Rejected != 1 {
			t.Fatalf("partial recovery: records=%v error=%v", records, err)
		}
		if strings.Contains(err.Error(), "credential-canary") {
			t.Fatal("record contents leaked")
		}
		preserved, err := store.Client.CoreV1().ConfigMaps("ci").Get(context.Background(), unknown.Name, metav1.GetOptions{})
		if err != nil || !reflect.DeepEqual(preserved, unknown) {
			t.Fatal("unsupported record changed")
		}
	}
}

func TestRecordSchemasRoundTripAndRejectUnknownFields(t *testing.T) {
	for _, version := range []int{1, 2} {
		record := engine.Record{Version: version, ID: strings.Repeat("a", 32), State: engine.StateReady}
		cm, err := recordMap(record)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parseRecord(cm)
		if err != nil || !reflect.DeepEqual(parsed, record) {
			t.Fatalf("v%d record mismatch: %+v %v", version, parsed, err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(cm.Data[recordKey]), &payload); err != nil {
			t.Fatal(err)
		}
		payload["futureField"] = "credential-canary"
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		cm.Data[recordKey] = string(data)
		if _, err := parseRecord(cm); err == nil || strings.Contains(err.Error(), "credential-canary") {
			t.Fatalf("unsafe unknown-field handling: %v", err)
		}
	}
}

func TestAgentErrorsDiscardUntrustedDetails(t *testing.T) {
	const canary = "credential-canary"
	err := safeAgentError(errors.New(canary))
	if err == nil || strings.Contains(err.Error(), canary) || errors.Unwrap(err) != nil {
		t.Fatalf("raw error retained: %v", err)
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(safeAgentError(cause), cause) {
			t.Fatalf("context classification lost: %v", cause)
		}
	}
}
