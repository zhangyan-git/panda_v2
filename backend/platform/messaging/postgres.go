package messaging

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGQuerier is implemented by both *pgxpool.Pool and pgx.Tx. Passing a
// transaction allows the application write and outbox append to commit
// atomically.
type PGQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// PostgreSQL stores outbox and inbox state durably in PostgreSQL. The tables
// are created by the messaging migration in this package's migrations dir.
type PostgreSQL struct {
	q PGQuerier
}

func NewPostgreSQL(pool *pgxpool.Pool) *PostgreSQL { return &PostgreSQL{q: pool} }

// NewPostgreSQLWithQuerier is useful when application state and the outbox
// must use the same pgx transaction.
func NewPostgreSQLWithQuerier(q PGQuerier) *PostgreSQL { return &PostgreSQL{q: q} }

func (p *PostgreSQL) Append(ctx context.Context, event Envelope) error {
	if event.EventID == "" {
		return errors.New("messaging: event ID is required")
	}
	result, err := p.q.Exec(ctx, `INSERT INTO message_outbox
		(event_id,event_type,event_version,trace_id,payload)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (event_id) DO NOTHING`, event.EventID, event.EventType,
		event.EventVersion, event.TraceID, event.Payload)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 1 {
		return nil
	}
	var existing Envelope
	if err := p.q.QueryRow(ctx, `SELECT event_id,event_type,event_version,trace_id,payload
		FROM message_outbox WHERE event_id=$1`, event.EventID).Scan(
		&existing.EventID, &existing.EventType, &existing.EventVersion, &existing.TraceID, &existing.Payload); err != nil {
		return err
	}
	if existing.EventType != event.EventType || existing.EventVersion != event.EventVersion || existing.TraceID != event.TraceID || string(existing.Payload) != string(event.Payload) {
		return fmt.Errorf("messaging: outbox event %q conflicts with existing event", event.EventID)
	}
	return nil
}

func (p *PostgreSQL) Pending(ctx context.Context, limit int) ([]Envelope, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.q.Query(ctx, `SELECT event_id,event_type,event_version,trace_id,payload
		FROM message_outbox
		WHERE published_at IS NULL AND next_attempt_at <= NOW()
		ORDER BY created_at,event_id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Envelope, 0, limit)
	for rows.Next() {
		var e Envelope
		if err := rows.Scan(&e.EventID, &e.EventType, &e.EventVersion, &e.TraceID, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (p *PostgreSQL) MarkPublished(ctx context.Context, eventID string) error {
	result, err := p.q.Exec(ctx, `UPDATE message_outbox
		SET published_at=NOW(), attempts=attempts+1
		WHERE event_id=$1 AND published_at IS NULL`, eventID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("messaging: outbox event %q is missing or already published", eventID)
	}
	return nil
}

func (p *PostgreSQL) Claim(ctx context.Context, eventID string) (bool, error) {
	if eventID == "" {
		return false, errors.New("messaging: event ID is required")
	}
	result, err := p.q.Exec(ctx, `INSERT INTO message_inbox (event_id)
		VALUES ($1) ON CONFLICT (event_id) DO NOTHING`, eventID)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() == 1, nil
}

func (p *PostgreSQL) ClaimPending(ctx context.Context, limit int, owner string, lease time.Duration) ([]LeasedEnvelope, error) {
	if limit <= 0 {
		limit = 100
	}
	if owner == "" || lease <= 0 {
		return nil, errors.New("messaging: invalid outbox lease")
	}
	token := uuid.NewString()
	rows, err := p.q.Query(ctx, `WITH claimed AS (
		SELECT event_id FROM message_outbox
		WHERE published_at IS NULL AND next_attempt_at <= NOW()
		  AND (lease_until IS NULL OR lease_until <= NOW())
		ORDER BY created_at,event_id LIMIT $1 FOR UPDATE SKIP LOCKED
	)
	UPDATE message_outbox o SET lease_owner=$2, lease_token=$3, lease_until=NOW()+make_interval(secs => $4), attempts=attempts+1
	FROM claimed WHERE o.event_id=claimed.event_id
	RETURNING o.event_id,o.event_type,o.event_version,o.trace_id,o.payload,o.lease_owner,o.lease_token,o.lease_until`, limit, owner, token, lease.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]LeasedEnvelope, 0, limit)
	for rows.Next() {
		var e LeasedEnvelope
		if err := rows.Scan(&e.EventID, &e.EventType, &e.EventVersion, &e.TraceID, &e.Payload, &e.LeaseOwner, &e.LeaseToken, &e.LeaseUntil); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func (p *PostgreSQL) MarkSuccess(ctx context.Context, eventID, token string) error {
	result, err := p.q.Exec(ctx, `UPDATE message_outbox SET published_at=NOW(), lease_owner=NULL, lease_token=NULL, lease_until=NULL WHERE event_id=$1 AND published_at IS NULL AND lease_token=$2`, eventID, token)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("messaging: outbox event %q lease is not owned", eventID)
	}
	return nil
}

const maxOutboxErrorSummary = 1024

var (
	credentialPattern    = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|authorization)\s*[:=]\s*[^\s,;]+`)
	urlCredentialPattern = regexp.MustCompile(`(?i)(://[^/:\s]+:)[^@\s]+@`)
)

func sanitizeOutboxError(failure error) string {
	if failure == nil {
		return ""
	}
	// Persist only a bounded summary. Redact credential-like fields because
	// broker/database errors may echo connection strings or request headers.
	summary := strings.TrimSpace(failure.Error())
	summary = credentialPattern.ReplaceAllString(summary, "$1=[REDACTED]")
	summary = urlCredentialPattern.ReplaceAllString(summary, "$1[REDACTED]@")
	if len(summary) > maxOutboxErrorSummary {
		summary = summary[:maxOutboxErrorSummary]
	}
	return summary
}

func (p *PostgreSQL) MarkFailure(ctx context.Context, eventID, token string, failure error, next time.Time) error {
	lastError := sanitizeOutboxError(failure)
	result, err := p.q.Exec(ctx, `UPDATE message_outbox SET next_attempt_at=$3, last_error=$4, lease_owner=NULL, lease_token=NULL, lease_until=NULL WHERE event_id=$1 AND published_at IS NULL AND lease_token=$2`, eventID, token, next, lastError)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("messaging: outbox event %q lease is not owned", eventID)
	}
	return nil
}

func (p *PostgreSQL) ClaimDurable(ctx context.Context, eventID, owner string, lease time.Duration) (bool, string, error) {
	if eventID == "" || owner == "" || lease <= 0 {
		return false, "", errors.New("messaging: invalid inbox lease")
	}
	token := uuid.NewString()
	result, err := p.q.Exec(ctx, `INSERT INTO message_inbox (event_id,lease_owner,lease_token,lease_until) VALUES ($1,$2,$3,NOW()+make_interval(secs => $4))
		ON CONFLICT (event_id) DO UPDATE SET lease_owner=EXCLUDED.lease_owner, lease_token=EXCLUDED.lease_token, lease_until=EXCLUDED.lease_until
		WHERE message_inbox.completed_at IS NULL AND (message_inbox.lease_until IS NULL OR message_inbox.lease_until <= NOW())`, eventID, owner, token, lease.Seconds())
	return err == nil && result.RowsAffected() == 1, token, err
}

func (p *PostgreSQL) Complete(ctx context.Context, eventID, token string) error {
	result, err := p.q.Exec(ctx, `UPDATE message_inbox SET completed_at=NOW(), lease_owner=NULL, lease_token=NULL, lease_until=NULL WHERE event_id=$1 AND completed_at IS NULL AND lease_token=$2`, eventID, token)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("messaging: inbox event %q lease is not owned", eventID)
	}
	return nil
}

func (p *PostgreSQL) Release(ctx context.Context, eventID, token string) error {
	result, err := p.q.Exec(ctx, `UPDATE message_inbox SET lease_owner=NULL, lease_token=NULL, lease_until=NULL WHERE event_id=$1 AND completed_at IS NULL AND lease_token=$2`, eventID, token)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("messaging: inbox event %q lease is not owned", eventID)
	}
	return nil
}

var _ Outbox = (*PostgreSQL)(nil)
var _ Inbox = (*PostgreSQL)(nil)
var _ DurableOutbox = (*PostgreSQL)(nil)
var _ DurableInbox = (*PostgreSQL)(nil)
var _ PGQuerier = (*pgxpool.Pool)(nil)
