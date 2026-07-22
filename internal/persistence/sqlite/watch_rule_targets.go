package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sephriot/code-reviewer/internal/application/watchrule"
)

// ErrAutomaticWatchRuleTargetNotFound means no current verified canonical
// evidence remains for the requested pull request.
var ErrAutomaticWatchRuleTargetNotFound = errors.New("automatic watch rule target not found")

// AutomaticWatchRuleTarget is current verified PR evidence with active
// immutable rule versions. It carries no scheduling or publication authority.
type AutomaticWatchRuleTarget struct {
	ConnectionID  string
	PullRequestID string
	Canonical     CanonicalReviewTarget
	Facts         watchrule.Facts
	Rules         []AutomaticWatchRule
}

// AutomaticWatchRule is one active current immutable rule version.
type AutomaticWatchRule struct {
	PolicySetID          string
	RuleID               string
	VersionID            string
	RuleKey              string
	Priority             int
	TriggerKind          string
	ExternalActionPolicy string
	ProfileID            string
	ProfileVersionID     string
	MatchJSON            []byte
	ReviewJSON           []byte
	PublicationJSON      []byte
}

// LoadAutomaticWatchRuleTarget reads selected canonical evidence, current PR
// facts, and all enabled current rule versions in one read-only snapshot.
func (s *Store) LoadAutomaticWatchRuleTarget(ctx context.Context, connectionID, pullRequestID string) (AutomaticWatchRuleTarget, error) {
	connectionID = strings.TrimSpace(connectionID)
	pullRequestID = strings.TrimSpace(pullRequestID)
	if connectionID == "" || pullRequestID == "" {
		return AutomaticWatchRuleTarget{}, errors.New("automatic watch rule connection and pull request IDs are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return AutomaticWatchRuleTarget{}, fmt.Errorf("begin automatic watch rule read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	canonical, err := loadCurrentCanonicalReviewTarget(ctx, tx, connectionID, pullRequestID)
	if errors.Is(err, ErrCanonicalReviewTargetNotFound) {
		return AutomaticWatchRuleTarget{}, ErrAutomaticWatchRuleTargetNotFound
	}
	if err != nil {
		return AutomaticWatchRuleTarget{}, fmt.Errorf("load automatic watch rule canonical target: %w", err)
	}
	facts, err := loadAutomaticWatchRuleFacts(ctx, tx, canonical)
	if err != nil {
		return AutomaticWatchRuleTarget{}, err
	}
	rules, err := loadAutomaticWatchRules(ctx, tx, facts)
	if err != nil {
		return AutomaticWatchRuleTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return AutomaticWatchRuleTarget{}, fmt.Errorf("commit automatic watch rule read: %w", err)
	}
	return AutomaticWatchRuleTarget{
		ConnectionID: connectionID, PullRequestID: pullRequestID,
		Canonical: canonical, Facts: facts, Rules: rules,
	}, nil
}

func loadAutomaticWatchRuleFacts(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, canonical CanonicalReviewTarget) (watchrule.Facts, error) {
	var facts watchrule.Facts
	var labelsJSON, relationshipsJSON []byte
	err := queryer.QueryRowContext(ctx, `
SELECT repository.github_id, repository.full_name, observation.author_login,
       observation.labels_json, observation.relationship_set_json,
       observation.is_draft, observation.github_state, observation.base_ref
FROM pull_request_projection_state AS projection
JOIN repositories AS repository ON repository.id = projection.repository_id
JOIN pull_request_observations AS observation
  ON observation.id = projection.current_observation_id
 AND observation.connection_id = projection.connection_id
 AND observation.pull_request_id = projection.pull_request_id
WHERE projection.connection_id = ?
  AND projection.pull_request_id = ?
  AND projection.current_observation_id = ?
  AND projection.current_revision_id = ?`,
		canonical.ConnectionID, canonical.PullRequestID, canonical.ObservationID, canonical.RevisionID,
	).Scan(&facts.RepositoryID, &facts.RepositoryFullName, &facts.AuthorLogin,
		&labelsJSON, &relationshipsJSON, &facts.IsDraft, &facts.State, &facts.BaseRef)
	if errors.Is(err, sql.ErrNoRows) {
		return watchrule.Facts{}, ErrAutomaticWatchRuleTargetNotFound
	}
	if err != nil {
		return watchrule.Facts{}, fmt.Errorf("load automatic watch rule facts: %w", err)
	}
	labels, err := decodeAutomaticWatchRuleFactSet(labelsJSON)
	if err != nil {
		return watchrule.Facts{}, fmt.Errorf("stored watch rule labels are invalid: %w", err)
	}
	relationships, err := decodeAutomaticWatchRuleFactSet(relationshipsJSON)
	if err != nil {
		return watchrule.Facts{}, fmt.Errorf("stored watch rule relationships are invalid: %w", err)
	}
	facts.Labels, facts.Relationships = labels, relationships
	if err := validateAutomaticWatchRuleFacts(facts); err != nil {
		return watchrule.Facts{}, err
	}
	return facts, nil
}

func decodeAutomaticWatchRuleFactSet(raw []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var values []string
	if err := decoder.Decode(&values); err != nil || values == nil {
		return nil, errors.New("must be a JSON string array")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	result := make([]string, len(values))
	copy(result, values)
	return result, nil
}

func validateAutomaticWatchRuleFacts(facts watchrule.Facts) error {
	if facts.RepositoryID <= 0 || strings.TrimSpace(facts.RepositoryFullName) == "" ||
		strings.TrimSpace(facts.AuthorLogin) == "" || strings.TrimSpace(facts.State) == "" ||
		strings.TrimSpace(facts.BaseRef) == "" {
		return errors.New("stored watch rule facts are incomplete")
	}
	match := map[string]any{
		"repository_ids":   []int64{facts.RepositoryID},
		"repository_names": []string{facts.RepositoryFullName},
		"authors":          []string{facts.AuthorLogin},
		"is_draft":         facts.IsDraft,
		"states":           []string{facts.State},
		"base_refs":        []string{facts.BaseRef},
	}
	if len(facts.Labels) > 0 {
		match["labels"] = facts.Labels
	}
	if len(facts.Relationships) > 0 {
		match["relationships"] = facts.Relationships
	}
	encoded, err := json.Marshal(match)
	if err != nil {
		return fmt.Errorf("encode watch rule fact validation: %w", err)
	}
	matched, err := watchrule.Match(facts, encoded)
	if err != nil || !matched {
		return errors.New("stored watch rule facts are invalid")
	}
	return nil
}

func loadAutomaticWatchRules(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, facts watchrule.Facts) ([]AutomaticWatchRule, error) {
	rows, err := queryer.QueryContext(ctx, `
SELECT version.policy_set_id, rule.id, version.id, rule.rule_key, version.priority,
       version.trigger_kind, version.external_action_policy,
       version.profile_id, version.profile_version_id,
       version.match_json, version.review_json, version.publication_json
FROM watch_rules AS rule
JOIN watch_rule_versions AS version
  ON version.id = rule.current_version_id AND version.rule_id = rule.id
WHERE rule.enabled = 1
ORDER BY version.priority, rule.id, version.id`)
	if err != nil {
		return nil, fmt.Errorf("load automatic watch rules: %w", err)
	}
	defer rows.Close()

	rules := make([]AutomaticWatchRule, 0)
	for rows.Next() {
		var rule AutomaticWatchRule
		if err := rows.Scan(&rule.PolicySetID, &rule.RuleID, &rule.VersionID, &rule.RuleKey, &rule.Priority,
			&rule.TriggerKind, &rule.ExternalActionPolicy,
			&rule.ProfileID, &rule.ProfileVersionID,
			&rule.MatchJSON, &rule.ReviewJSON, &rule.PublicationJSON); err != nil {
			return nil, fmt.Errorf("scan automatic watch rule: %w", err)
		}
		if err := validateAutomaticWatchRule(rule, facts); err != nil {
			return nil, err
		}
		rule.MatchJSON = append([]byte(nil), rule.MatchJSON...)
		rule.ReviewJSON = append([]byte(nil), rule.ReviewJSON...)
		rule.PublicationJSON = append([]byte(nil), rule.PublicationJSON...)
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate automatic watch rules: %w", err)
	}
	return rules, nil
}

func validateAutomaticWatchRule(rule AutomaticWatchRule, facts watchrule.Facts) error {
	if rule.PolicySetID == "" || rule.RuleID == "" || rule.VersionID == "" || rule.RuleKey == "" || rule.Priority < 0 ||
		!validPolicyTriggerKind(rule.TriggerKind) || !validExternalActionPolicy(rule.ExternalActionPolicy) ||
		((rule.ProfileID == "") != (rule.ProfileVersionID == "")) ||
		((rule.TriggerKind == "automatic" || rule.TriggerKind == "manual") && rule.ProfileID == "") ||
		((rule.TriggerKind == "ignore" || rule.TriggerKind == "track_only") && rule.ProfileID != "") {
		return errors.New("stored automatic watch rule is invalid")
	}
	for _, value := range []*[]byte{&rule.MatchJSON, &rule.ReviewJSON, &rule.PublicationJSON} {
		normalized, err := normalizePolicyJSONObject(*value)
		if err != nil || !bytes.Equal(normalized, *value) {
			return errors.New("stored automatic watch rule JSON is invalid")
		}
	}
	if _, err := watchrule.Match(facts, rule.MatchJSON); err != nil {
		return errors.New("stored automatic watch rule match is invalid")
	}
	return nil
}
