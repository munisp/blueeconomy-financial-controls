package outbox

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresSource drains the transactional outboxes in creation order.
type PostgresSource struct {
	pool *pgxpool.Pool
}

func NewPostgresSource(pool *pgxpool.Pool) *PostgresSource { return &PostgresSource{pool: pool} }

// outboxTable is one drainable outbox source.
type outboxTable struct {
	name          string
	subjectColumn string
}

var tables = []outboxTable{
	{name: "financial_intent_outbox", subjectColumn: "intent_id"},
	{name: "cvff_outbox", subjectColumn: "application_id"},
	// W-FEAT-7 revenue-assurance outbox (deferred registration, W-CLOSE-FC).
	{name: "revenue_outbox", subjectColumn: "subject_id"},
	// WP-6 trade-finance rail outbox.
	{name: "tf_outbox", subjectColumn: "subject_id"},
}

// Unpublished returns up to limit unpublished events across both outboxes,
// oldest first.
func (source *PostgresSource) Unpublished(ctx context.Context, limit int) ([]Event, error) {
	events := make([]Event, 0)
	remaining := limit
	for _, table := range tables {
		if remaining <= 0 {
			break
		}
		rows, err := source.pool.Query(ctx, fmt.Sprintf(`
			SELECT event_id, %s, event_type, payload, created_at
			FROM %s WHERE published_at IS NULL ORDER BY created_at LIMIT $1`, table.subjectColumn, table.name), remaining)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", table.name, err)
		}
		for rows.Next() {
			var event Event
			if err := rows.Scan(&event.EventID, &event.SubjectID, &event.EventType, &event.Payload, &event.CreatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s row: %w", table.name, err)
			}
			event.table = table
			events = append(events, event)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate %s: %w", table.name, err)
		}
		remaining = limit - len(events)
	}
	return events, nil
}

// MarkPublished stamps one event; it fails when the row is absent or already
// published.
func (source *PostgresSource) MarkPublished(ctx context.Context, event Event) error {
	result, err := source.pool.Exec(ctx, fmt.Sprintf(`UPDATE %s SET published_at = $1 WHERE event_id = $2 AND published_at IS NULL`, event.table.name), time.Now().UTC(), event.EventID)
	if err != nil {
		return fmt.Errorf("mark %s published: %w", event.EventID, err)
	}
	if result.RowsAffected() != 1 {
		return pgx.ErrNoRows
	}
	return nil
}
