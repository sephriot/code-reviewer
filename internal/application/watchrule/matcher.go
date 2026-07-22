// Package watchrule selects an enabled watch rule from current pull-request facts.
package watchrule

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var (
	// ErrInvalidRule reports a rule that cannot safely participate in selection.
	ErrInvalidRule = errors.New("watch rule is invalid")
	// ErrInvalidMatch reports malformed or unsupported match JSON.
	ErrInvalidMatch = errors.New("watch rule match is invalid")
	// ErrInvalidFacts reports facts that cannot safely be matched.
	ErrInvalidFacts = errors.New("pull request facts are invalid")
)

// Facts is the current, canonical pull-request state used for watch-rule selection.
// RepositoryID is GitHub's numeric repository ID.
type Facts struct {
	RepositoryID       int64
	RepositoryFullName string
	AuthorLogin        string
	Labels             []string
	Relationships      []string
	IsDraft            bool
	State              string
	BaseRef            string
}

// Rule is one immutable watch-rule version eligible for selection.
type Rule struct {
	ID        string
	Enabled   bool
	Priority  int
	MatchJSON []byte
}

// Selection is first matching enabled rule. Found is false when no rule matches.
type Selection struct {
	Rule  Rule
	Found bool
}

// Match reports whether strict matchJSON matches facts. Match JSON must be an
// object containing only: relationships, repository_ids, repository_names,
// authors, labels, is_draft, states, and base_refs. Array predicates are
// non-empty; relationships and labels require every listed value, while the
// other arrays allow any listed value. All configured predicates combine with
// AND. Empty object matches every valid fact set.
func Match(facts Facts, matchJSON []byte) (bool, error) {
	match, err := parseMatch(matchJSON)
	if err != nil {
		return false, err
	}
	return match.matches(facts)
}

// Select evaluates enabled rules by ascending priority then stable rule ID.
func Select(facts Facts, rules []Rule) (Selection, error) {
	enabled := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if strings.TrimSpace(rule.ID) == "" || rule.Priority < 0 {
			return Selection{}, fmt.Errorf("%w: id or priority", ErrInvalidRule)
		}
		if _, err := parseMatch(rule.MatchJSON); err != nil {
			return Selection{}, err
		}
		enabled = append(enabled, rule)
	}
	sort.Slice(enabled, func(i, j int) bool {
		if enabled[i].Priority != enabled[j].Priority {
			return enabled[i].Priority < enabled[j].Priority
		}
		return enabled[i].ID < enabled[j].ID
	})
	for _, rule := range enabled {
		matched, err := Match(facts, rule.MatchJSON)
		if err != nil {
			return Selection{}, err
		}
		if matched {
			return Selection{Rule: rule, Found: true}, nil
		}
	}
	return Selection{}, nil
}

type match struct {
	relationships   []string
	repositoryIDs   []int64
	repositoryNames []string
	authors         []string
	labels          []string
	isDraft         *bool
	states          []string
	baseRefs        []string
}

func parseMatch(input []byte) (match, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		return match{}, invalidMatch("empty JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return match{}, invalidMatch("must be an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return match{}, invalidMatch("invalid object key")
		}
		if _, exists := fields[key]; exists {
			return match{}, invalidMatch("duplicate predicate " + key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return match{}, invalidMatch("invalid value for " + key)
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return match{}, invalidMatch("unterminated object")
	}
	if err := requireEOF(decoder); err != nil {
		return match{}, invalidMatch("trailing JSON")
	}
	result := match{}
	for key, value := range fields {
		var err error
		switch key {
		case "relationships":
			result.relationships, err = parseStringList(value, normalizeRelationship)
		case "repository_ids":
			result.repositoryIDs, err = parseRepositoryIDs(value)
		case "repository_names":
			result.repositoryNames, err = parseStringList(value, normalizeRepositoryName)
		case "authors":
			result.authors, err = parseStringList(value, normalizeIdentity)
		case "labels":
			result.labels, err = parseStringList(value, normalizeIdentity)
		case "is_draft":
			result.isDraft, err = parseBool(value)
		case "states":
			result.states, err = parseStringList(value, normalizeState)
		case "base_refs":
			result.baseRefs, err = parseStringList(value, normalizeBaseRef)
		default:
			return match{}, invalidMatch("unsupported predicate " + key)
		}
		if err != nil {
			return match{}, invalidMatch(key + ": " + err.Error())
		}
	}
	return result, nil
}

func (match match) matches(facts Facts) (bool, error) {
	if len(match.relationships) > 0 {
		relationships, err := normalizedFactsSet(facts.Relationships, normalizeRelationship)
		if err != nil {
			return false, invalidFacts("relationships: " + err.Error())
		}
		if !containsAll(relationships, match.relationships) {
			return false, nil
		}
	}
	if len(match.repositoryIDs) > 0 {
		if facts.RepositoryID <= 0 {
			return false, invalidFacts("repository ID")
		}
		if !slices.Contains(match.repositoryIDs, facts.RepositoryID) {
			return false, nil
		}
	}
	if len(match.repositoryNames) > 0 {
		name, err := normalizeRepositoryName(facts.RepositoryFullName)
		if err != nil {
			return false, invalidFacts("repository name: " + err.Error())
		}
		if !slices.Contains(match.repositoryNames, name) {
			return false, nil
		}
	}
	if len(match.authors) > 0 {
		author, err := normalizeIdentity(facts.AuthorLogin)
		if err != nil {
			return false, invalidFacts("author: " + err.Error())
		}
		if !slices.Contains(match.authors, author) {
			return false, nil
		}
	}
	if len(match.labels) > 0 {
		labels, err := normalizedFactsSet(facts.Labels, normalizeIdentity)
		if err != nil {
			return false, invalidFacts("labels: " + err.Error())
		}
		if !containsAll(labels, match.labels) {
			return false, nil
		}
	}
	if match.isDraft != nil && facts.IsDraft != *match.isDraft {
		return false, nil
	}
	if len(match.states) > 0 {
		state, err := normalizeState(facts.State)
		if err != nil {
			return false, invalidFacts("state: " + err.Error())
		}
		if !slices.Contains(match.states, state) {
			return false, nil
		}
	}
	if len(match.baseRefs) > 0 {
		baseRef, err := normalizeBaseRef(facts.BaseRef)
		if err != nil {
			return false, invalidFacts("base ref: " + err.Error())
		}
		if !slices.Contains(match.baseRefs, baseRef) {
			return false, nil
		}
	}
	return true, nil
}

func parseStringList(input json.RawMessage, normalize func(string) (string, error)) ([]string, error) {
	var values []string
	if err := json.Unmarshal(input, &values); err != nil || len(values) == 0 {
		return nil, errors.New("must be a non-empty string array")
	}
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		item, err := normalize(value)
		if err != nil {
			return nil, err
		}
		if slices.Contains(normalized, item) {
			return nil, errors.New("duplicate value")
		}
		normalized = append(normalized, item)
	}
	return normalized, nil
}

func parseRepositoryIDs(input json.RawMessage) ([]int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var values []json.Number
	if err := decoder.Decode(&values); err != nil || len(values) == 0 || requireEOF(decoder) != nil {
		return nil, errors.New("must be a non-empty integer array")
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseInt(value.String(), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("must contain positive integers")
		}
		if slices.Contains(ids, id) {
			return nil, errors.New("duplicate value")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseBool(input json.RawMessage) (*bool, error) {
	switch string(bytes.TrimSpace(input)) {
	case "true":
		value := true
		return &value, nil
	case "false":
		value := false
		return &value, nil
	default:
		return nil, errors.New("must be boolean")
	}
}

func normalizedFactsSet(values []string, normalize func(string) (string, error)) ([]string, error) {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		item, err := normalize(value)
		if err != nil {
			return nil, err
		}
		if slices.Contains(normalized, item) {
			return nil, errors.New("duplicate value")
		}
		normalized = append(normalized, item)
	}
	return normalized, nil
}

func containsAll(actual, expected []string) bool {
	for _, value := range expected {
		if !slices.Contains(actual, value) {
			return false
		}
	}
	return true
}

func normalizeRelationship(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "review_requested" && value != "authored_by_me" {
		return "", errors.New("unsupported relationship")
	}
	return value, nil
}

func normalizeRepositoryName(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validAtom(parts[0]) || !validAtom(parts[1]) {
		return "", errors.New("must be owner/name")
	}
	return value, nil
}

func normalizeIdentity(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !validAtom(value) {
		return "", errors.New("must be non-empty text without whitespace")
	}
	return value, nil
}

func normalizeState(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "open", "closed", "merged":
		return value, nil
	default:
		return "", errors.New("unsupported state")
	}
}

func normalizeBaseRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !validAtom(value) {
		return "", errors.New("must be non-empty text without whitespace")
	}
	return value, nil
}

func validAtom(value string) bool {
	return value != "" && !strings.ContainsFunc(value, unicode.IsSpace)
}

func invalidMatch(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalidMatch, detail)
}

func invalidFacts(detail string) error {
	return fmt.Errorf("%w: %s", ErrInvalidFacts, detail)
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
