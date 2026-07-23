package watchrule

import (
	"errors"
	"testing"
)

func TestMatchRequiresEveryConfiguredPredicate(t *testing.T) {
	facts := testFacts()
	match := []byte(`{
		"relationships":["review_requested","authored_by_me"],
		"repository_ids":[42,43],
		"repository_names":["other/repo","Owner/Repo"],
		"authors":["nobody","OCTOCAT"],
		"labels":["security","Go"],
		"is_draft":false,
		"states":["open"],
		"base_refs":["release","main"]
	}`)

	matched, err := Match(facts, match)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected all predicates to match")
	}

	facts.Labels = []string{"security"}
	matched, err = Match(facts, match)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("missing required label must not match")
	}
}

func TestMatchSupportsAnyValueWithinPredicate(t *testing.T) {
	matched, err := Match(testFacts(), []byte(`{
		"repository_ids":[7,42],
		"repository_names":["other/repo","owner/repo"],
		"authors":["other","octocat"],
		"states":["closed","open"],
		"base_refs":["release","main"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected one allowed value per predicate to match")
	}
}

func TestMatchSupportsOrganizationRepositoryWildcard(t *testing.T) {
	facts := testFacts()
	facts.RepositoryFullName = "Spacelift-IO/backend"
	matched, err := Match(facts, []byte(`{"repository_names":["spacelift-io/*"]}`))
	if err != nil || !matched {
		t.Fatalf("matched=%t err=%v", matched, err)
	}
	facts.RepositoryFullName = "other/backend"
	matched, err = Match(facts, []byte(`{"repository_names":["spacelift-io/*"]}`))
	if err != nil || matched {
		t.Fatalf("cross-organization matched=%t err=%v", matched, err)
	}
}

func TestMatchEmptyObjectMatches(t *testing.T) {
	matched, err := Match(testFacts(), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("empty match object must match")
	}
}

func TestMatchRejectsMalformedOrUnsupportedMatches(t *testing.T) {
	tests := []struct {
		name  string
		match string
	}{
		{name: "invalid json", match: `{`},
		{name: "not object", match: `[]`},
		{name: "unknown field", match: `{"title":"x"}`},
		{name: "duplicate predicate", match: `{"authors":["octocat"],"authors":["other"]}`},
		{name: "null field", match: `{"authors":null}`},
		{name: "null draft", match: `{"is_draft":null}`},
		{name: "empty array", match: `{"authors":[]}`},
		{name: "duplicate array value", match: `{"authors":["octocat","OCTOCAT"]}`},
		{name: "invalid relationship", match: `{"relationships":["assigned"]}`},
		{name: "invalid state", match: `{"states":["active"]}`},
		{name: "nonpositive repository id", match: `{"repository_ids":[0]}`},
		{name: "fractional repository id", match: `{"repository_ids":[4.2]}`},
		{name: "bad repository name", match: `{"repository_names":["owner"]}`},
		{name: "empty base ref", match: `{"base_refs":[""]}`},
		{name: "wrong draft type", match: `{"is_draft":"false"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Match(testFacts(), []byte(test.match))
			if !errors.Is(err, ErrInvalidMatch) {
				t.Fatalf("error = %v, want ErrInvalidMatch", err)
			}
		})
	}
}

func TestMatchRejectsInvalidFactsWhenPredicateNeedsThem(t *testing.T) {
	tests := []struct {
		name  string
		facts Facts
		match string
	}{
		{name: "repository id", facts: Facts{RepositoryFullName: "owner/repo"}, match: `{"repository_ids":[42]}`},
		{name: "repository name", facts: Facts{RepositoryID: 42}, match: `{"repository_names":["owner/repo"]}`},
		{name: "author", facts: Facts{RepositoryID: 42, RepositoryFullName: "owner/repo"}, match: `{"authors":["octocat"]}`},
		{name: "state", facts: Facts{RepositoryID: 42, RepositoryFullName: "owner/repo", AuthorLogin: "octocat"}, match: `{"states":["open"]}`},
		{name: "base ref", facts: Facts{RepositoryID: 42, RepositoryFullName: "owner/repo", AuthorLogin: "octocat", State: "open"}, match: `{"base_refs":["main"]}`},
		{name: "relationship", facts: Facts{RepositoryID: 42, RepositoryFullName: "owner/repo", AuthorLogin: "octocat", State: "open", BaseRef: "main", Relationships: []string{"bad"}}, match: `{"relationships":["review_requested"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Match(test.facts, []byte(test.match))
			if !errors.Is(err, ErrInvalidFacts) {
				t.Fatalf("error = %v, want ErrInvalidFacts", err)
			}
		})
	}
}

func TestSelectUsesPriorityThenStableIDAndSkipsDisabledRules(t *testing.T) {
	selection, err := Select(testFacts(), []Rule{
		{ID: "z-disabled", Enabled: false, Priority: 0, MatchJSON: []byte(`{}`)},
		{ID: "z-same-priority", Enabled: true, Priority: 10, MatchJSON: []byte(`{}`)},
		{ID: "a-same-priority", Enabled: true, Priority: 10, MatchJSON: []byte(`{}`)},
		{ID: "later", Enabled: true, Priority: 20, MatchJSON: []byte(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Found || selection.Rule.ID != "a-same-priority" {
		t.Fatalf("selection = %+v, want a-same-priority", selection)
	}
}

func TestSelectReturnsNoRuleAndDoesNotMutateInput(t *testing.T) {
	rules := []Rule{
		{ID: "later", Enabled: true, Priority: 20, MatchJSON: []byte(`{"authors":["other"]}`)},
		{ID: "first", Enabled: true, Priority: 10, MatchJSON: []byte(`{"labels":["missing"]}`)},
	}
	selection, err := Select(testFacts(), rules)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Found {
		t.Fatalf("selection = %+v, want none", selection)
	}
	if rules[0].ID != "later" || rules[1].ID != "first" {
		t.Fatal("Select mutated caller rule order")
	}
}

func TestSelectFailsClosedForInvalidEnabledRule(t *testing.T) {
	_, err := Select(testFacts(), []Rule{
		{ID: "valid", Enabled: true, Priority: 1, MatchJSON: []byte(`{}`)},
		{ID: "bad", Enabled: true, Priority: 2, MatchJSON: []byte(`{"unknown":true}`)},
	})
	if !errors.Is(err, ErrInvalidMatch) {
		t.Fatalf("error = %v, want ErrInvalidMatch", err)
	}
}

func TestSelectRejectsInvalidEnabledRuleMetadata(t *testing.T) {
	tests := []Rule{
		{Enabled: true, Priority: 0, MatchJSON: []byte(`{}`)},
		{ID: "negative", Enabled: true, Priority: -1, MatchJSON: []byte(`{}`)},
	}
	for _, rule := range tests {
		_, err := Select(testFacts(), []Rule{rule})
		if !errors.Is(err, ErrInvalidRule) {
			t.Fatalf("error = %v, want ErrInvalidRule", err)
		}
	}
}

func testFacts() Facts {
	return Facts{
		RepositoryID:       42,
		RepositoryFullName: "owner/repo",
		AuthorLogin:        "octocat",
		Labels:             []string{"security", "go"},
		Relationships:      []string{"review_requested", "authored_by_me"},
		IsDraft:            false,
		State:              "open",
		BaseRef:            "main",
	}
}
