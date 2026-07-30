package web

import (
	"strings"
)

var outcomeLabels = map[string]string{
	"approve_without_comments": "Approve without comments",
	"approve_with_comments":    "Approve with comments",
	"changes_requested":        "Changes requested",
	"human_review":             "Human review",
	"tool_failed":              "Tool failed",
	"reviewed_externally":      "Reviewed externally",
}

var filterLabels = map[string]string{
	"author": "author filter",
	"draft":  "draft",
	"repo":   "repo filter",
}

func formatOutcome(outcome string) string {
	if label, ok := outcomeLabels[outcome]; ok {
		return label
	}
	return strings.ReplaceAll(outcome, "_", " ")
}

func formatFilter(reason string) string {
	if label, ok := filterLabels[reason]; ok {
		return label
	}
	return reason
}

func formatStatus(status string) string {
	return strings.ReplaceAll(status, "_", " ")
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func showHistorySummary(outcome, summary string) bool {
	s := strings.TrimSpace(summary)
	if s == "" {
		return false
	}
	label := formatOutcome(outcome)
	if strings.EqualFold(s, label) {
		return false
	}
	if strings.EqualFold(s, formatStatus(outcome)) {
		return false
	}
	return true
}
