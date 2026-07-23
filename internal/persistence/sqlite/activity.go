package sqlite

import (
	"context"
	"fmt"
	"time"
)

// ActivityItem is a safe, durable runtime record for local diagnostics.
type ActivityItem struct {
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	State      string    `json:"state"`
	Detail     string    `json:"detail"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ListActivity returns recent durable job and event state without payloads.
func (s *Store) ListActivity(ctx context.Context, limit int) ([]ActivityItem, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("activity limit is invalid")
	}
	rows, err := s.db.QueryContext(ctx, "SELECT kind, id, state, detail, occurred_at_us FROM (SELECT 'job', id, state, kind, updated_at_us FROM jobs UNION ALL SELECT 'event', id, 'recorded', event_type, occurred_at_us FROM domain_events UNION ALL SELECT 'outbox', id, state, topic, updated_at_us FROM outbox) ORDER BY occurred_at_us DESC, id DESC LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()
	items := make([]ActivityItem, 0, limit)
	for rows.Next() {
		var item ActivityItem
		var at int64
		if err := rows.Scan(&item.Kind, &item.ID, &item.State, &item.Detail, &at); err != nil {
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
