package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxPolicyRuleJSONBytes = 64 * 1024

// ErrPolicySetGenerationConflict means one generation is already bound to
// different immutable policy content.
var ErrPolicySetGenerationConflict = errors.New("policy set generation content conflict")

// PolicySetGenerationInput is one complete, immutable policy generation.
// Rules omitted from a new generation are disabled and have their current
// version pointer cleared; prior rule versions and policy sets remain intact.
type PolicySetGenerationInput struct {
	Generation int
	Rules      []WatchRuleVersionInput
	CreatedAt  time.Time
}

// WatchRuleVersionInput supplies the immutable policy content for one stable
// rule in a policy generation.
type WatchRuleVersionInput struct {
	RuleKey              string
	Enabled              bool
	Priority             int
	TriggerKind          string
	ExternalActionPolicy string
	ProfileID            string
	ProfileVersionID     string
	MatchJSON            []byte
	ReviewJSON           []byte
	PublicationJSON      []byte
}

// PolicySetGenerationResult identifies a persisted immutable generation.
type PolicySetGenerationResult struct {
	PolicySetID   string
	Generation    int
	ContentSHA256 string
	RuleVersions  []WatchRuleVersionResult
	Created       bool
}

// WatchRuleVersionResult identifies one stable rule and its immutable version.
type WatchRuleVersionResult struct {
	RuleID        string
	VersionID     string
	RuleKey       string
	Version       int
	ContentSHA256 string
}

// CreatePolicySetGeneration stores a complete immutable policy generation.
// Exact retries return existing IDs without changing stable rule pointers.
func (s *Store) CreatePolicySetGeneration(ctx context.Context, input PolicySetGenerationInput) (PolicySetGenerationResult, error) {
	normalized, err := normalizePolicySetGenerationInput(input)
	if err != nil {
		return PolicySetGenerationResult{}, err
	}
	result := PolicySetGenerationResult{
		PolicySetID:   stableID("policy-set", strconv.Itoa(normalized.Generation), normalized.ContentSHA256),
		Generation:    normalized.Generation,
		ContentSHA256: normalized.ContentSHA256,
		RuleVersions:  policyRuleResults(normalized),
	}

	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		existing, found, err := loadPolicySet(ctx, conn, normalized.Generation)
		if err != nil {
			return err
		}
		if found {
			if existing.ID != result.PolicySetID || existing.ContentSHA256 != normalized.ContentSHA256 {
				return fmt.Errorf("%w: generation=%d", ErrPolicySetGenerationConflict, normalized.Generation)
			}
			if err := requireStoredPolicyRules(ctx, conn, existing.ID, normalized); err != nil {
				return fmt.Errorf("%w: generation=%d", ErrPolicySetGenerationConflict, normalized.Generation)
			}
			return nil
		}

		if _, err := conn.ExecContext(ctx, `
INSERT INTO policy_sets(id, generation, content_sha256, created_at_us)
VALUES (?, ?, ?, ?)`, result.PolicySetID, normalized.Generation, normalized.ContentSHA256, normalized.CreatedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert policy set: %w", err)
		}
		for _, rule := range normalized.Rules {
			if err := ensurePolicyWatchRule(ctx, conn, rule, normalized.CreatedAt); err != nil {
				return err
			}
			if rule.ProfileID != "" {
				if err := requireReviewProfileVersion(ctx, conn, rule.ProfileID, rule.ProfileVersionID); err != nil {
					return fmt.Errorf("watch rule %q profile: %w", rule.RuleKey, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `
INSERT INTO watch_rule_versions(
 id, rule_id, policy_set_id, version, priority, trigger_kind, external_action_policy,
 profile_id, profile_version_id, match_json, review_json, publication_json,
 content_sha256, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)`,
				rule.VersionID, rule.RuleID, result.PolicySetID, normalized.Generation, rule.Priority,
				rule.TriggerKind, rule.ExternalActionPolicy, rule.ProfileID, rule.ProfileVersionID,
				rule.MatchJSON, rule.ReviewJSON, rule.PublicationJSON, rule.ContentSHA256, normalized.CreatedAt.UnixMicro()); err != nil {
				return fmt.Errorf("insert watch rule version %q: %w", rule.RuleKey, err)
			}
			if err := updatePolicyWatchRule(ctx, conn, rule.RuleID, rule.Enabled, rule.VersionID, normalized.CreatedAt); err != nil {
				return err
			}
		}
		if err := disableRulesOmittedFromPolicySet(ctx, conn, result.PolicySetID, normalized.CreatedAt); err != nil {
			return err
		}
		result.Created = true
		return nil
	})
	if err != nil {
		return PolicySetGenerationResult{}, fmt.Errorf("create policy set generation: %w", err)
	}
	return result, nil
}

type normalizedPolicySetGenerationInput struct {
	Generation    int
	Rules         []normalizedWatchRuleVersionInput
	ContentSHA256 string
	CreatedAt     time.Time
}

type normalizedWatchRuleVersionInput struct {
	RuleKey              string
	RuleID               string
	VersionID            string
	Enabled              bool
	Priority             int
	TriggerKind          string
	ExternalActionPolicy string
	ProfileID            string
	ProfileVersionID     string
	MatchJSON            []byte
	ReviewJSON           []byte
	PublicationJSON      []byte
	ContentSHA256        string
}

func normalizePolicySetGenerationInput(input PolicySetGenerationInput) (normalizedPolicySetGenerationInput, error) {
	if input.Generation <= 0 || len(input.Rules) == 0 {
		return normalizedPolicySetGenerationInput{}, errors.New("policy set generation input is invalid")
	}
	createdAt := input.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if createdAt.UnixMicro() < 0 {
		return normalizedPolicySetGenerationInput{}, errors.New("policy set created time is invalid")
	}
	rules := make([]normalizedWatchRuleVersionInput, 0, len(input.Rules))
	seenKeys := make(map[string]struct{}, len(input.Rules))
	seenPriorities := make(map[int]struct{}, len(input.Rules))
	for _, rawRule := range input.Rules {
		rule, err := normalizeWatchRuleVersionInput(rawRule, input.Generation)
		if err != nil {
			return normalizedPolicySetGenerationInput{}, err
		}
		if _, exists := seenKeys[rule.RuleKey]; exists {
			return normalizedPolicySetGenerationInput{}, errors.New("policy set has duplicate rule key")
		}
		if _, exists := seenPriorities[rule.Priority]; exists {
			return normalizedPolicySetGenerationInput{}, errors.New("policy set has duplicate rule priority")
		}
		seenKeys[rule.RuleKey] = struct{}{}
		seenPriorities[rule.Priority] = struct{}{}
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Priority != rules[j].Priority {
			return rules[i].Priority < rules[j].Priority
		}
		return rules[i].RuleKey < rules[j].RuleKey
	})
	content, err := json.Marshal(struct {
		FormatVersion int                               `json:"format_version"`
		Generation    int                               `json:"generation"`
		Rules         []normalizedWatchRuleVersionInput `json:"rules"`
	}{FormatVersion: 1, Generation: input.Generation, Rules: rules})
	if err != nil {
		return normalizedPolicySetGenerationInput{}, fmt.Errorf("encode policy set content: %w", err)
	}
	digest := sha256.Sum256(content)
	return normalizedPolicySetGenerationInput{
		Generation: input.Generation, Rules: rules, ContentSHA256: hex.EncodeToString(digest[:]), CreatedAt: createdAt,
	}, nil
}

func normalizeWatchRuleVersionInput(input WatchRuleVersionInput, generation int) (normalizedWatchRuleVersionInput, error) {
	ruleKey := strings.ToLower(normalizeReviewProfileText(input.RuleKey))
	triggerKind := strings.TrimSpace(input.TriggerKind)
	externalActionPolicy := strings.TrimSpace(input.ExternalActionPolicy)
	profileID := strings.TrimSpace(input.ProfileID)
	profileVersionID := strings.TrimSpace(input.ProfileVersionID)
	if !validReviewProfileKey(ruleKey) || input.Priority < 0 || !validPolicyTriggerKind(triggerKind) ||
		!validExternalActionPolicy(externalActionPolicy) ||
		((profileID == "") != (profileVersionID == "")) ||
		((triggerKind == "automatic" || triggerKind == "manual") && profileID == "") ||
		((triggerKind == "ignore" || triggerKind == "track_only") && profileID != "") {
		return normalizedWatchRuleVersionInput{}, errors.New("watch rule version input is invalid")
	}
	match, err := normalizePolicyJSONObject(input.MatchJSON)
	if err != nil {
		return normalizedWatchRuleVersionInput{}, fmt.Errorf("watch rule match: %w", err)
	}
	review, err := normalizePolicyJSONObject(input.ReviewJSON)
	if err != nil {
		return normalizedWatchRuleVersionInput{}, fmt.Errorf("watch rule review: %w", err)
	}
	publication, err := normalizePolicyJSONObject(input.PublicationJSON)
	if err != nil {
		return normalizedWatchRuleVersionInput{}, fmt.Errorf("watch rule publication: %w", err)
	}
	ruleID := stableID("watch-rule", ruleKey)
	versionID := stableID("watch-rule-version", ruleID, strconv.Itoa(generation))
	content, err := json.Marshal(struct {
		FormatVersion        int             `json:"format_version"`
		RuleKey              string          `json:"rule_key"`
		Priority             int             `json:"priority"`
		TriggerKind          string          `json:"trigger_kind"`
		ExternalActionPolicy string          `json:"external_action_policy"`
		ProfileID            string          `json:"profile_id,omitempty"`
		ProfileVersionID     string          `json:"profile_version_id,omitempty"`
		Match                json.RawMessage `json:"match"`
		Review               json.RawMessage `json:"review"`
		Publication          json.RawMessage `json:"publication"`
	}{
		FormatVersion: 1, RuleKey: ruleKey, Priority: input.Priority, TriggerKind: triggerKind,
		ExternalActionPolicy: externalActionPolicy, ProfileID: profileID, ProfileVersionID: profileVersionID,
		Match: match, Review: review, Publication: publication,
	})
	if err != nil {
		return normalizedWatchRuleVersionInput{}, fmt.Errorf("encode watch rule content: %w", err)
	}
	digest := sha256.Sum256(content)
	return normalizedWatchRuleVersionInput{
		RuleKey: ruleKey, RuleID: ruleID, VersionID: versionID, Enabled: input.Enabled, Priority: input.Priority,
		TriggerKind: triggerKind, ExternalActionPolicy: externalActionPolicy, ProfileID: profileID, ProfileVersionID: profileVersionID,
		MatchJSON: match, ReviewJSON: review, PublicationJSON: publication, ContentSHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func normalizePolicyJSONObject(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if len(raw) > maxPolicyRuleJSONBytes {
		return nil, errors.New("JSON object exceeds maximum size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumePolicyJSONValue(decoder, nil); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("must contain one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, errors.New("must contain one JSON object")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain one JSON object")
	}
	var object map[string]any
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode JSON object: %w", err)
	}
	if len(normalized) > maxPolicyRuleJSONBytes {
		return nil, errors.New("JSON object exceeds maximum size")
	}
	return normalized, nil
}

func consumePolicyJSONValue(decoder *json.Decoder, keys map[string]struct{}) error {
	token, err := decoder.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("must contain one JSON object")
		}
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		keys = make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, exists := keys[key]; exists {
				return errors.New("JSON object has duplicate key")
			}
			keys[key] = struct{}{}
			if err := consumePolicyJSONValue(decoder, nil); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumePolicyJSONValue(decoder, nil); err != nil {
				return err
			}
		}
		_, err := decoder.Token()
		return err
	default:
		return errors.New("JSON delimiter is invalid")
	}
}

func validPolicyTriggerKind(value string) bool {
	return value == "ignore" || value == "track_only" || value == "automatic" || value == "manual"
}

func validExternalActionPolicy(value string) bool {
	return value == "advisory_only" || value == "require_confirmation" || value == "auto_publish" || value == "human_attention"
}

func policyRuleResults(input normalizedPolicySetGenerationInput) []WatchRuleVersionResult {
	result := make([]WatchRuleVersionResult, 0, len(input.Rules))
	for _, rule := range input.Rules {
		result = append(result, WatchRuleVersionResult{
			RuleID: rule.RuleID, VersionID: rule.VersionID, RuleKey: rule.RuleKey,
			Version: input.Generation, ContentSHA256: rule.ContentSHA256,
		})
	}
	return result
}

type storedPolicySet struct {
	ID            string
	ContentSHA256 string
}

func loadPolicySet(ctx context.Context, conn *sql.Conn, generation int) (storedPolicySet, bool, error) {
	var stored storedPolicySet
	err := conn.QueryRowContext(ctx, `SELECT id, content_sha256 FROM policy_sets WHERE generation = ?`, generation).Scan(&stored.ID, &stored.ContentSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return storedPolicySet{}, false, nil
	}
	if err != nil {
		return storedPolicySet{}, false, fmt.Errorf("load policy set: %w", err)
	}
	return stored, true, nil
}

func requireStoredPolicyRules(ctx context.Context, conn *sql.Conn, policySetID string, input normalizedPolicySetGenerationInput) error {
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM watch_rule_versions WHERE policy_set_id = ?`, policySetID).Scan(&count); err != nil {
		return err
	}
	if count != len(input.Rules) {
		return errors.New("policy set rule count differs")
	}
	for _, rule := range input.Rules {
		var stored normalizedWatchRuleVersionInput
		var storedProfileID, storedProfileVersionID sql.NullString
		err := conn.QueryRowContext(ctx, `
SELECT id, rule_id, priority, trigger_kind, external_action_policy,
       profile_id, profile_version_id, match_json, review_json, publication_json, content_sha256
FROM watch_rule_versions WHERE id = ? AND policy_set_id = ?`, rule.VersionID, policySetID).Scan(
			&stored.VersionID, &stored.RuleID, &stored.Priority, &stored.TriggerKind, &stored.ExternalActionPolicy,
			&storedProfileID, &storedProfileVersionID, &stored.MatchJSON, &stored.ReviewJSON, &stored.PublicationJSON, &stored.ContentSHA256,
		)
		if err != nil {
			return err
		}
		stored.RuleKey = rule.RuleKey
		stored.ProfileID, stored.ProfileVersionID = storedProfileID.String, storedProfileVersionID.String
		if stored.RuleID != rule.RuleID || stored.Priority != rule.Priority || stored.TriggerKind != rule.TriggerKind ||
			stored.ExternalActionPolicy != rule.ExternalActionPolicy || stored.ProfileID != rule.ProfileID || stored.ProfileVersionID != rule.ProfileVersionID ||
			!bytes.Equal(stored.MatchJSON, rule.MatchJSON) || !bytes.Equal(stored.ReviewJSON, rule.ReviewJSON) ||
			!bytes.Equal(stored.PublicationJSON, rule.PublicationJSON) || stored.ContentSHA256 != rule.ContentSHA256 {
			return errors.New("policy set rule differs")
		}
	}
	return nil
}

func ensurePolicyWatchRule(ctx context.Context, conn *sql.Conn, rule normalizedWatchRuleVersionInput, createdAt time.Time) error {
	var id, key string
	err := conn.QueryRowContext(ctx, `SELECT id, rule_key FROM watch_rules WHERE rule_key = ? COLLATE NOCASE`, rule.RuleKey).Scan(&id, &key)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := conn.ExecContext(ctx, `
INSERT INTO watch_rules(id, rule_key, enabled, current_version_id, created_at_us, updated_at_us)
VALUES (?, ?, ?, NULL, ?, ?)`, rule.RuleID, rule.RuleKey, boolToSQLite(rule.Enabled), createdAt.UnixMicro(), createdAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert watch rule %q: %w", rule.RuleKey, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("load watch rule %q: %w", rule.RuleKey, err)
	case id != rule.RuleID || key != rule.RuleKey:
		return fmt.Errorf("%w: watch rule key=%q", ErrPolicySetGenerationConflict, rule.RuleKey)
	default:
		return nil
	}
}

func updatePolicyWatchRule(ctx context.Context, conn *sql.Conn, ruleID string, enabled bool, versionID string, at time.Time) error {
	if _, err := conn.ExecContext(ctx, `
UPDATE watch_rules
SET enabled = ?, current_version_id = ?, updated_at_us = CASE WHEN updated_at_us > ? THEN updated_at_us ELSE ? END
WHERE id = ?`, boolToSQLite(enabled), versionID, at.UnixMicro(), at.UnixMicro(), ruleID); err != nil {
		return fmt.Errorf("update current watch rule version: %w", err)
	}
	return nil
}

func disableRulesOmittedFromPolicySet(ctx context.Context, conn *sql.Conn, policySetID string, at time.Time) error {
	if _, err := conn.ExecContext(ctx, `
UPDATE watch_rules
SET enabled = 0, current_version_id = NULL,
    updated_at_us = CASE WHEN updated_at_us > ? THEN updated_at_us ELSE ? END
WHERE id NOT IN (SELECT rule_id FROM watch_rule_versions WHERE policy_set_id = ?)`, at.UnixMicro(), at.UnixMicro(), policySetID); err != nil {
		return fmt.Errorf("disable omitted watch rules: %w", err)
	}
	return nil
}

func boolToSQLite(value bool) int {
	if value {
		return 1
	}
	return 0
}
