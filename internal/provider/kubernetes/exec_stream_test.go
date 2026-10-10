package kubernetes

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDrainExecOutputConsumesLargeTrailingFrame(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, `{"exit_code":0}`+"\n"+strings.Repeat("x", 256<<10))
		_ = writer.CloseWithError(err)
		done <- err
	}()
	decoder := json.NewDecoder(reader)
	var terminal map[string]any
	if err := decoder.Decode(&terminal); err != nil {
		t.Fatal(err)
	}
	type result struct {
		unexpected bool
		err        error
	}
	drained := make(chan result, 1)
	go func() {
		unexpected, err := drainExecOutput(decoder, reader)
		drained <- result{unexpected, err}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		_ = reader.Close()
		t.Fatal("writer remained blocked after terminal event")
	}
	got := <-drained
	if got.err != nil || !got.unexpected {
		t.Fatalf("trailing bytes: %+v", got)
	}
}

func TestDrainExecOutputAcceptsJSONWhitespace(t *testing.T) {
	reader := strings.NewReader(" \t\r\n")
	unexpected, err := drainExecOutput(json.NewDecoder(strings.NewReader("")), reader)
	if err != nil || unexpected {
		t.Fatalf("whitespace trailer: unexpected=%t err=%v", unexpected, err)
	}
}
