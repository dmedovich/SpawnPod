package engine

import (
	"testing"
	"time"
)

func TestPlanReconciliationMatchesRecoveryRules(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		record Record
		want   ReconcileAction
	}{
		{"ready", Record{State: StateReady, Deadline: now.Add(time.Hour)}, ReconcileRetain},
		{"expired ready", Record{State: StateReady, Deadline: now.Add(-time.Second)}, ReconcileRemove},
		{"interrupted exec", Record{State: StateExecuting, Deadline: now.Add(time.Hour)}, ReconcileRemove},
		{"recent tombstone", Record{State: StateCleaned, CleanedAt: now.Add(-time.Hour)}, ReconcileRetain},
		{"old tombstone", Record{State: StateCleaned, CleanedAt: now.Add(-24 * time.Hour)}, ReconcileExpireTombstone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if action, _ := PlanReconciliation(tc.record, now); action != tc.want {
				t.Fatalf("got %s, want %s", action, tc.want)
			}
		})
	}
}
