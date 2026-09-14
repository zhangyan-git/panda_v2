// Package audit turns administrative operations into durable outbox events.
//
// The audit row and the business write it describes have to land together. An
// operation that rolls back must not leave a log claiming it happened, and an
// operation that commits must not lose its log because the broker is down.
// Appending to the outbox inside the business transaction, and letting a relay
// publish from there, is what makes both true — writing the log directly would
// break the first, and publishing directly would break the second.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/trace"

	"github.com/panda-dev/panda-v2/backend/platform/auth"
	"github.com/panda-dev/panda-v2/backend/platform/messaging"
)

const (
	// EventType is both the outbox event type and the RabbitMQ routing key: the
	// broker derives one from the other (messaging.RabbitConfig.routeKey), so
	// changing this string moves the queue binding with it.
	EventType = "admin.operation.logged"

	// EventVersion versions the payload shape. Bump it when Entry changes in a
	// way a consumer built against the previous shape cannot read.
	EventVersion = "1"
)

// Entry is one administrative operation. The field set matches the identity
// database's admin_operation_logs columns because the consumer writes these
// values straight into that table, which makes the JSON tags a contract
// between the service that records and the service that persists.
//
// ActorID is a user id, not a username. Tokens carry no username, so the
// display fields (admin_username / admin_name) are resolved by the consumer
// from the id at write time. The id is the part that has to be right at
// operation time; the name is a convenience snapshot taken slightly later.
type Entry struct {
	ActorID      string          `json:"actor_id"`
	Module       string          `json:"module"`
	Action       string          `json:"action"`
	Operation    string          `json:"operation"`
	TargetType   string          `json:"target_type,omitempty"`
	TargetID     string          `json:"target_id,omitempty"`
	TargetName   string          `json:"target_name,omitempty"`
	MerchantID   string          `json:"merchant_id,omitempty"`
	Result       string          `json:"result,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	Before       json.RawMessage `json:"before_data,omitempty"`
	After        json.RawMessage `json:"after_data,omitempty"`
}

// Recorder appends an audit entry to the outbox. Implementations must write
// through the caller's transaction, never their own.
type Recorder interface {
	Record(ctx context.Context, tx pgx.Tx, entry Entry) error
}

// Noop records nothing. Repositories are wired with it when auditing is off, so
// that call sites stay unconditional and there is no branch to forget.
type Noop struct{}

func (Noop) Record(context.Context, pgx.Tx, Entry) error { return nil }

type outboxRecorder struct{}

// NewRecorder returns the Recorder that appends to the message outbox.
func NewRecorder() Recorder { return outboxRecorder{} }

func (outboxRecorder) Record(ctx context.Context, tx pgx.Tx, entry Entry) error {
	// A nil transaction means the caller assembled its own and lost it, or
	// never opened one. Appending on the pool here would silently defeat the
	// whole point of the outbox, so refuse instead of guessing.
	if tx == nil {
		return errors.New("audit: recording an entry requires the business transaction")
	}
	if entry.ActorID == "" {
		entry.ActorID = ActorIDFromContext(ctx)
	}
	if entry.Module == "" || entry.Action == "" {
		return fmt.Errorf("audit: entry needs a module and an action (module=%q action=%q)", entry.Module, entry.Action)
	}
	if entry.Result == "" {
		entry.Result = "success"
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("audit: encode entry: %w", err)
	}
	return messaging.NewPostgreSQLWithQuerier(tx).Append(ctx, messaging.Envelope{
		EventID:      uuid.NewString(),
		EventType:    EventType,
		EventVersion: EventVersion,
		TraceID:      TraceIDFromContext(ctx),
		Payload:      payload,
	})
}

// Snapshot encodes a value for the before_data / after_data columns.
//
// Callers pass a purpose-built struct with json tags and no secrets — never a
// domain model wholesale, which carries no json tags at all and, for accounts,
// carries the password hash. An unencodable value yields nil instead of an
// error: losing a snapshot is worth less than failing the operation it describes.
func Snapshot(value any) json.RawMessage {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

// Decode reads an Entry from a delivered envelope, rejecting events of any
// other type so that a queue shared with future event types cannot misroute
// one into the audit table.
func Decode(event messaging.Envelope) (Entry, error) {
	if event.EventType != EventType {
		return Entry{}, fmt.Errorf("audit: unexpected event type %q", event.EventType)
	}
	var entry Entry
	if err := json.Unmarshal(event.Payload, &entry); err != nil {
		return Entry{}, fmt.Errorf("audit: decode entry: %w", err)
	}
	return entry, nil
}

// ActorIDFromContext returns the acting user's id, or "" for an operation with
// no authenticated actor, such as a scheduled job.
func ActorIDFromContext(ctx context.Context) string {
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok {
		return ""
	}
	return identity.UserID
}

// TraceIDFromContext returns the current trace id, or "" when the operation is
// not traced. Carrying it into the outbox is what lets the consumer's work show
// up under the originating request in the tracing backend.
func TraceIDFromContext(ctx context.Context) string {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return ""
	}
	return spanContext.TraceID().String()
}
