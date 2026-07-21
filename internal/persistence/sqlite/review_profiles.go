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
	"strconv"
	"strings"
	"time"
)

const (
	maxReviewProfileKeyBytes          = 128
	maxReviewProfileNameBytes         = 512
	maxReviewProfileDescriptionBytes  = 16 * 1024
	maxReviewProfileInstructionsBytes = 64 * 1024
	maxReviewProfileSettingsBytes     = 64 * 1024
)

// ErrReviewProfileVersionConflict means a profile key and version are already
// bound to different immutable content.
var ErrReviewProfileVersionConflict = errors.New("review profile version content conflict")

// CreateReviewProfileVersionInput supplies one immutable review profile
// version. A profile key is stable across versions; callers select a new,
// positive version number whenever its content changes.
type CreateReviewProfileVersionInput struct {
	ProfileKey   string
	Version      int
	Name         string
	Description  string
	Instructions string
	SettingsJSON []byte
	CreatedAt    time.Time
}

// CreateReviewProfileVersionResult identifies a durable profile version.
type CreateReviewProfileVersionResult struct {
	ProfileID     string
	VersionID     string
	ProfileKey    string
	Version       int
	ContentSHA256 string
	Created       bool
}

// CreateReviewProfileVersion stores an immutable profile version. Exact
// repeated input is idempotent. Reusing a profile key and version with changed
// content fails closed rather than silently rewriting review behavior.
func (s *Store) CreateReviewProfileVersion(ctx context.Context, input CreateReviewProfileVersionInput) (CreateReviewProfileVersionResult, error) {
	normalized, err := normalizeReviewProfileVersionInput(input)
	if err != nil {
		return CreateReviewProfileVersionResult{}, err
	}

	result := CreateReviewProfileVersionResult{
		ProfileID:     stableID("review-profile", normalized.ProfileKey),
		ProfileKey:    normalized.ProfileKey,
		Version:       normalized.Version,
		ContentSHA256: normalized.ContentSHA256,
	}
	result.VersionID = stableID("review-profile-version", result.ProfileID, strconv.Itoa(result.Version))

	err = withImmediateConnection(ctx, s.db, func(conn *sql.Conn) error {
		if err := ensureReviewProfile(ctx, conn, result.ProfileID, normalized.ProfileKey, normalized.CreatedAt); err != nil {
			return err
		}
		existing, found, err := loadReviewProfileVersion(ctx, conn, result.ProfileID, normalized.Version)
		if err != nil {
			return err
		}
		if found {
			if existing.ID != result.VersionID || !existing.matches(normalized) {
				return fmt.Errorf("%w: profile_key=%q version=%d", ErrReviewProfileVersionConflict, normalized.ProfileKey, normalized.Version)
			}
			return nil
		}
		if _, err := conn.ExecContext(ctx, `
INSERT INTO review_profile_versions(
 id, profile_id, version, name, description, instructions, output_schema_version,
 settings_json, content_sha256, created_at_us)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			result.VersionID, result.ProfileID, normalized.Version, normalized.Name,
			normalized.Description, normalized.Instructions, normalized.SettingsJSON,
			normalized.ContentSHA256, normalized.CreatedAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert review profile version: %w", err)
		}
		result.Created = true
		return nil
	})
	if err != nil {
		return CreateReviewProfileVersionResult{}, fmt.Errorf("create review profile version: %w", err)
	}
	return result, nil
}

type normalizedReviewProfileVersionInput struct {
	ProfileKey    string
	Version       int
	Name          string
	Description   string
	Instructions  string
	SettingsJSON  []byte
	ContentSHA256 string
	CreatedAt     time.Time
}

func normalizeReviewProfileVersionInput(input CreateReviewProfileVersionInput) (normalizedReviewProfileVersionInput, error) {
	profileKey := strings.ToLower(normalizeReviewProfileText(input.ProfileKey))
	name := normalizeReviewProfileText(input.Name)
	description := normalizeReviewProfileText(input.Description)
	instructions := normalizeReviewProfileText(input.Instructions)
	if !validReviewProfileKey(profileKey) || input.Version <= 0 ||
		name == "" || instructions == "" ||
		len(name) > maxReviewProfileNameBytes ||
		len(description) > maxReviewProfileDescriptionBytes ||
		len(instructions) > maxReviewProfileInstructionsBytes {
		return normalizedReviewProfileVersionInput{}, errors.New("review profile version input is invalid")
	}
	settings, err := normalizeReviewProfileSettings(input.SettingsJSON)
	if err != nil {
		return normalizedReviewProfileVersionInput{}, fmt.Errorf("review profile settings: %w", err)
	}
	content, err := json.Marshal(struct {
		FormatVersion       int             `json:"format_version"`
		Name                string          `json:"name"`
		Description         string          `json:"description"`
		Instructions        string          `json:"instructions"`
		OutputSchemaVersion int             `json:"output_schema_version"`
		Settings            json.RawMessage `json:"settings"`
	}{
		FormatVersion:       1,
		Name:                name,
		Description:         description,
		Instructions:        instructions,
		OutputSchemaVersion: 1,
		Settings:            settings,
	})
	if err != nil {
		return normalizedReviewProfileVersionInput{}, fmt.Errorf("encode review profile version content: %w", err)
	}
	digest := sha256.Sum256(content)
	createdAt := input.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if createdAt.UnixMicro() < 0 {
		return normalizedReviewProfileVersionInput{}, errors.New("review profile created time is invalid")
	}
	return normalizedReviewProfileVersionInput{
		ProfileKey: profileKey, Version: input.Version, Name: name, Description: description,
		Instructions: instructions, SettingsJSON: settings,
		ContentSHA256: hex.EncodeToString(digest[:]), CreatedAt: createdAt,
	}, nil
}

func normalizeReviewProfileText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func validReviewProfileKey(value string) bool {
	if value == "" || len(value) > maxReviewProfileKeyBytes {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func normalizeReviewProfileSettings(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if len(raw) > maxReviewProfileSettingsBytes {
		return nil, errors.New("JSON object exceeds maximum size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil || !errors.Is(err, io.EOF) {
		return nil, errors.New("must contain one JSON object")
	}
	normalized, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode JSON object: %w", err)
	}
	if len(normalized) > maxReviewProfileSettingsBytes {
		return nil, errors.New("JSON object exceeds maximum size")
	}
	return normalized, nil
}

func ensureReviewProfile(ctx context.Context, conn *sql.Conn, profileID, profileKey string, createdAt time.Time) error {
	var existingID, existingKey string
	err := conn.QueryRowContext(ctx, `
SELECT id, profile_key FROM review_profiles WHERE profile_key = ? COLLATE NOCASE`, profileKey).Scan(&existingID, &existingKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := conn.ExecContext(ctx, `
INSERT INTO review_profiles(id, profile_key, created_at_us) VALUES (?, ?, ?)`, profileID, profileKey, createdAt.UnixMicro()); err != nil {
			return fmt.Errorf("insert review profile: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("load review profile: %w", err)
	case existingID != profileID || existingKey != profileKey:
		return fmt.Errorf("%w: profile_key=%q", ErrReviewProfileVersionConflict, profileKey)
	default:
		return nil
	}
}

type storedReviewProfileVersion struct {
	ID            string
	Name          string
	Description   string
	Instructions  string
	SettingsJSON  []byte
	ContentSHA256 string
}

func loadReviewProfileVersion(ctx context.Context, conn *sql.Conn, profileID string, version int) (storedReviewProfileVersion, bool, error) {
	var stored storedReviewProfileVersion
	err := conn.QueryRowContext(ctx, `
SELECT id, name, description, instructions, settings_json, content_sha256
FROM review_profile_versions
WHERE profile_id = ? AND version = ?`, profileID, version).Scan(
		&stored.ID, &stored.Name, &stored.Description, &stored.Instructions, &stored.SettingsJSON, &stored.ContentSHA256,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedReviewProfileVersion{}, false, nil
	}
	if err != nil {
		return storedReviewProfileVersion{}, false, fmt.Errorf("load review profile version: %w", err)
	}
	return stored, true, nil
}

func (stored storedReviewProfileVersion) matches(input normalizedReviewProfileVersionInput) bool {
	return stored.Name == input.Name &&
		stored.Description == input.Description &&
		stored.Instructions == input.Instructions &&
		bytes.Equal(stored.SettingsJSON, input.SettingsJSON) &&
		stored.ContentSHA256 == input.ContentSHA256
}
