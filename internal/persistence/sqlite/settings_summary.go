package sqlite

import (
	"context"
	"errors"
	"fmt"
)

// SettingsSummary contains non-secret control-plane configuration state for
// local operator visibility. It excludes credentials, endpoint locations,
// engine arguments, and rule/profile content.
type SettingsSummary struct {
	PublicationMode    PublicationMode
	ActiveWatchRules   int
	ConfiguredProfiles int
}

// SettingsSummary reads durable publication safety mode plus configured
// profile and active rule counts. It performs one SELECT-only statement.
func (s *Store) SettingsSummary(ctx context.Context) (SettingsSummary, error) {
	var summary SettingsSummary
	err := s.db.QueryRowContext(ctx, `
WITH configured_profiles AS (
 SELECT COUNT(*) AS count
 FROM review_profiles AS profile
 WHERE EXISTS (
   SELECT 1 FROM review_profile_versions AS version
   WHERE version.profile_id = profile.id
 )
), active_rules AS (
 SELECT COUNT(*) AS count
 FROM watch_rules AS rule
 JOIN watch_rule_versions AS version
   ON version.id = rule.current_version_id AND version.rule_id = rule.id
 WHERE rule.enabled = 1
)
SELECT state.value, active_rules.count, configured_profiles.count
FROM system_state AS state
CROSS JOIN active_rules
CROSS JOIN configured_profiles
WHERE state.key = 'publication_mode'`).Scan(
		&summary.PublicationMode, &summary.ActiveWatchRules, &summary.ConfiguredProfiles,
	)
	if err != nil {
		return SettingsSummary{}, fmt.Errorf("read settings summary: %w", err)
	}
	if summary.PublicationMode != PublicationModeDisabled && summary.PublicationMode != PublicationModeSimulated {
		return SettingsSummary{}, errors.New("stored publication mode is unsafe")
	}
	if summary.ActiveWatchRules < 0 || summary.ConfiguredProfiles < 0 {
		return SettingsSummary{}, errors.New("stored settings counts are invalid")
	}
	return summary, nil
}
