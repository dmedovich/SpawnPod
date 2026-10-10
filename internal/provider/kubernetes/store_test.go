package kubernetes

import (
	"strings"
	"testing"

	"github.com/dmedovich/spawnpod/internal/engine"
)

func TestServiceRecordVersionRoundTrip(t *testing.T) {
	id := strings.Repeat("a", 32)
	record := engine.Record{
		Version: 2, ID: id,
		Services: []engine.ServiceRecord{{
			Spec:   engine.ServiceSpec{EnvironmentID: id, Name: "redis", Image: "approved"},
			Handle: engine.ServiceHandle{Name: "sp-" + id + "-redis", ServiceUID: "service-uid"},
		}},
	}
	configMap, err := recordMap(record)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseRecord(configMap)
	if err != nil || parsed.Version != 2 || len(parsed.Services) != 1 || parsed.Services[0].Handle.ServiceUID != "service-uid" {
		t.Fatalf("round trip=%+v err=%v", parsed, err)
	}
	record.Version = 1
	if _, err := recordMap(record); err == nil {
		t.Fatal("version 1 record with services was accepted")
	}
	configMap.Data[recordKey] = strings.Replace(configMap.Data[recordKey], `"version":2`, `"version":1`, 1)
	if _, err := parseRecord(configMap); err == nil {
		t.Fatal("version 1 payload with services was accepted")
	}
}
