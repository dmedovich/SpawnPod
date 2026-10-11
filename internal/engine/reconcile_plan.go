package engine

import "time"

const DefaultTombstoneRetention = 24 * time.Hour

// ReconcileAction is the action the controller would take for an idle record.
// An active operation in the current process always takes precedence.
type ReconcileAction string

const (
	ReconcileRetain          ReconcileAction = "retain"
	ReconcileRemove          ReconcileAction = "remove"
	ReconcileExpireTombstone ReconcileAction = "expire-tombstone"
)

// PlanReconciliation is shared by the controller and the read-only cleanup
// preview. It makes no API calls and does not account for live operations.
func PlanReconciliation(record Record, now time.Time) (ReconcileAction, string) {
	return PlanReconciliationWithRetention(record, now, DefaultTombstoneRetention)
}

// PlanReconciliationWithRetention is also used by inspection and cleanup previews.
func PlanReconciliationWithRetention(record Record, now time.Time, retention time.Duration) (ReconcileAction, string) {
	if retention <= 0 {
		retention = DefaultTombstoneRetention
	}
	if record.State == StateCleaned {
		if !record.CleanedAt.IsZero() && now.Sub(record.CleanedAt) >= retention {
			return ReconcileExpireTombstone, "tombstone retention expired"
		}
		return ReconcileRetain, "tombstone retention"
	}
	if now.After(record.Deadline) {
		return ReconcileRemove, "environment deadline expired"
	}
	switch record.State {
	case StateProvisioning, StateExecuting, StateTransferring, StateIndeterminate, StateTerminating, StateFailed:
		return ReconcileRemove, "interrupted or failed environment"
	default:
		return ReconcileRetain, "environment is ready"
	}
}
