package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
)

// Run sends one request and calls onEvent until a terminal event arrives.
// Cancelling ctx closes the socket, which also cancels the remote command.
func Run(ctx context.Context, socket string, req Request, onEvent func(Event) error) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-finished:
		}
	}()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	dec := json.NewDecoder(conn)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("supervisor stream ended without result: %w", err)
		}
		if err := onEvent(ev); err != nil {
			return err
		}
		if ev.Error != "" {
			return errors.New(ev.Error)
		}
		if ev.ExitCode != nil {
			return nil
		}
	}
}
