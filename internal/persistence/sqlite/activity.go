package sqlite

import (
	"context"
	"fmt"
	"time"
)

const maxActivityErrorMessageBytes = 512

// ActivityItem is a safe, durable runtime record for local diagnostics.
type ActivityItem struct {
	Kind         string    `json:"kind"`
	ID           string    `json:"id"`
	State        string    `json:"state"`
	Detail       string    `json:"detail"`
	ErrorClass   string    `json:"error_class,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	OccurredAt   time.Time `json:"occurred_at"`
}

// ListActivity returns recent durable job and event state without payloads.
func (s *Store) ListActivity(ctx context.Context, limit int) ([]ActivityItem, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("activity limit is invalid")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT kind, id, state, detail, error_class, error_message, occurred_at_us
FROM (
 SELECT 'job' AS kind, id, state, kind AS detail,
        COALESCE(last_error_class, '') AS error_class,
        substr(COALESCE(last_error_message, ''), 1, ?) AS error_message,
        updated_at_us AS occurred_at_us
 FROM jobs
 UNION ALL
 SELECT 'event' AS kind, id, 'recorded' AS state, event_type AS detail,
        '' AS error_class, '' AS error_message, occurred_at_us
 FROM domain_events
 UNION ALL
 SELECT 'outbox' AS kind, id, state, topic AS detail,
        '' AS error_class, '' AS error_message, updated_at_us AS occurred_at_us
 FROM outbox
)
ORDER BY occurred_at_us DESC, id DESC
LIMIT ?`, maxActivityErrorMessageBytes, limit)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()
	items := make([]ActivityItem, 0, limit)
	for rows.Next() {
		var item ActivityItem
		var at int64
		if err := rows.Scan(&item.Kind, &item.ID, &item.State, &item.Detail, &item.ErrorClass, &item.ErrorMessage, &at); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		item.OccurredAt = time.UnixMicro(at).UTC()
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity: %w", err)
	}
	return items, nil
}
