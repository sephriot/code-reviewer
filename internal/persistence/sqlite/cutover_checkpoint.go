package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CutoverCheckpoint struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	PublicationMode     PublicationMode `json:"publication_mode"`
	DomainEventSequence int64           `json:"domain_event_sequence"`
	CreatedAt           time.Time       `json:"created_at"`
}

// CreateCutoverCheckpoint pins ledger position only while publication is disabled.
func (s *Store) CreateCutoverCheckpoint(ctx context.Context, name string, now time.Time) (CutoverCheckpoint, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 256 || now.IsZero() {
		return CutoverCheckpoint{}, errors.New("cutover checkpoint input is invalid")
	}
	result := CutoverCheckpoint{}
	err := withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		mode, err := loadSafePublicationMode(ctx, conn)
		if err != nil {
			return err
		}
		if mode != PublicationModeDisabled {
			return errors.New("cutover checkpoint requires disabled publication")
		}
		var sequence int64
		if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM domain_events`).Scan(&sequence); err != nil {
			return err
		}
		id, err := newID("cutover-checkpoint")
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO cutover_checkpoints(id, name, publication_mode, domain_event_sequence, created_at_us) VALUES (?, ?, ?, ?, ?)`, id, name, mode, sequence, now.UTC().UnixMicro()); err != nil {
			return fmt.Errorf("insert cutover checkpoint: %w", err)
		}
		result = CutoverCheckpoint{ID: id, Name: name, PublicationMode: mode, DomainEventSequence: sequence, CreatedAt: now.UTC()}
		return nil
	})
	return result, err
}
