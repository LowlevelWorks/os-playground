package session

import (
	"context"
	"encoding/json"
	"time"
)

// Audit event types (ADR 0010, sections 3 and 5). Lifecycle events are audit
// events that carry a session ID; there is no separate events table.
const (
	EventCreated         = "PLAYGROUND_CREATED"
	EventDeleteRequested = "PLAYGROUND_DELETE_REQUESTED"
	EventExpired         = "PLAYGROUND_EXPIRED"
	EventStatusChanged   = "PLAYGROUND_STATUS_CHANGED"
)

// AuditEvent is one row of audit_events. Stores write them in the same
// transaction as the change they describe, so an event exists exactly when
// its change does. Refused requests are not audited: they change nothing,
// and logs and metrics cover them.
type AuditEvent struct {
	OccurredAt time.Time
	Type       string
	RequestID  string // empty for the reconciler's own events
	UserID     string // the session's owner_id
	SessionID  string
	Metadata   map[string]string
}

// metadataJSON encodes m for the jsonb column. A map of strings always
// marshals, so the error is impossible.
func metadataJSON(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, _ := json.Marshal(m)
	return b
}

type requestIDKey struct{}

// WithRequestID returns ctx carrying the request's ID, which stores record
// in the audit events the request causes.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the ID WithRequestID stored, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// createdEvent and the helpers below build each event the same way in
// every store.
func createdEvent(ctx context.Context, s Session) AuditEvent {
	return AuditEvent{
		Type: EventCreated, RequestID: RequestID(ctx), UserID: s.OwnerID, SessionID: s.ID,
		Metadata: map[string]string{"profile": s.Profile, "image_ref": s.ImageRef},
	}
}

func deleteRequestedEvent(ctx context.Context, id, ownerID string) AuditEvent {
	return AuditEvent{Type: EventDeleteRequested, RequestID: RequestID(ctx), UserID: ownerID, SessionID: id}
}

// completeEvents is what one Complete records, given the row before it.
func completeEvents(id, ownerID string, old Status, oldDeleted bool, r Result) []AuditEvent {
	var out []AuditEvent
	if r.Status != "" && r.Status != old {
		m := map[string]string{"from": string(old), "to": string(r.Status)}
		if r.FailureReason != "" {
			m["reason"] = r.FailureReason
		}
		out = append(out, AuditEvent{Type: EventStatusChanged, UserID: ownerID, SessionID: id, Metadata: m})
	}
	if r.ExpireReason != "" && !oldDeleted {
		out = append(out, AuditEvent{
			Type: EventExpired, UserID: ownerID, SessionID: id,
			Metadata: map[string]string{"reason": r.ExpireReason},
		})
	}
	return out
}
