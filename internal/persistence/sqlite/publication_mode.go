package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetPublicationMode applies one runtime-configured safety mode and records a
// durable audit row only when it changes. It never releases existing effects.
func (s *Store) SetPublicationMode(ctx context.Context, mode PublicationMode, changedAt time.Time) (bool, error) {
	if mode != PublicationModeDisabled && mode != PublicationModeSimulated && mode != PublicationModeEnabled {
		return false, errors.New("publication mode is invalid")
	}
	if changedAt.IsZero() {
		changedAt = time.Now().UTC()
	} else {
		changedAt = changedAt.UTC()
	}
	if changedAt.UnixMicro() < 0 {
		return false, errors.New("publication mode time is invalid")
	}
	changed := false
	err := withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		current, err := loadSafePublicationMode(ctx, conn)
		if err != nil {
			return err
		}
		if current == mode {
			return nil
		}
		id, err := newID("publication-mode-change")
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE system_state SET value = ?, updated_at_us = ? WHERE key = 'publication_mode'`, mode, changedAt.UnixMicro()); err != nil {
			return fmt.Errorf("update publication mode: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO publication_mode_changes(id, previous_mode, new_mode, source, changed_at_us) VALUES (?, ?, ?, 'runtime_config', ?)`, id, current, mode, changedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert publication mode audit: %w", err)
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("set publication mode: %w", err)
	}
	return changed, nil
}
