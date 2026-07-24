package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

type ToolOutput struct {
	Action   string `json:"action"`
	Comment  string `json:"comment"`
	Summary  string `json:"summary"`
	Reason   string `json:"reason"`
	Comments []struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Message string `json:"message"`
	} `json:"comments"`
}

type Runner struct {
	cfg *config.Config
}

func NewRunner(cfg *config.Config) *Runner {
	return &Runner{cfg: cfg}
}

var outputFormatPrompt = `RESPOND WITH JSON ONLY - No analysis, explanation, or text before/after the JSON.

You MUST respond with JSON in this exact format:
{
  "action": "approve_with_comment" | "approve_without_comment" | "request_changes" | "requires_human_review",
  "comment": "Professional, constructive approval comment focusing on strengths and minor suggestions",
  "summary": "Comprehensive summary of issues requiring changes, organized by priority",
  "reason": "Detailed explanation of why human expertise is needed for this review",
  "comments": [
    {
      "file": "path/to/file.py",
      "line": 42,
      "message": "Specific, actionable feedback with suggested solutions and rationale"
    }
  ]
}

Action Decision Framework:
- APPROVE_WITHOUT_COMMENT: Perfect code that requires no feedback. Exemplary implementation following all best practices.
- APPROVE_WITH_COMMENT: Good code with constructive suggestions. Solid implementation with minor improvement opportunities.
- REQUEST_CHANGES: Code with significant issues that must be addressed (security, performance, design, quality issues).
- REQUIRES_HUMAN_REVIEW: Complex scenarios requiring domain expertise (architectural decisions, business logic complexity, technical complexity).

Quality Thresholds:
- APPROVE_WITHOUT_COMMENT: Code that could serve as an example for other developers
- APPROVE_WITH_COMMENT: Code that's ready to ship with minor suggestions for improvement
- REQUEST_CHANGES: Code that has fixable issues preventing safe deployment
- REQUIRES_HUMAN_REVIEW: Code that needs specialized expertise or represents significant risk

For inline comments, the "line" field must be the actual line number in the new version of the file.`

func (r *Runner) RunReview(ctx context.Context, prID int64, promptPath string) (*db.Review, error) {
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("read prompt file %s: %w", promptPath, err)
	}

	fullPrompt := string(prompt) + "\n\n" + outputFormatPrompt

	output, err := r.executeTool(ctx, fullPrompt)
	if err != nil {
		return nil, err
	}

	toolOut, err := parseOutput(output)
	if err != nil {
		return nil, fmt.Errorf("parse tool output: %w", err)
	}

	outcome := mapActionToOutcome(toolOut.Action)
	review := &db.Review{
		PullRequestID:  prID,
		Outcome:        outcome,
		Summary:        toolOut.Summary,
		GeneralComment: toolOut.Comment,
	}

	return review, nil
}

func (r *Runner) executeTool(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.ReviewTimeout)
	defer cancel()

	tool := strings.ToUpper(r.cfg.ReviewTool)
	switch tool {
	case "CLAUDE":
		return r.execClaude(ctx, prompt)
	case "CODEX":
		return r.execCodex(ctx, prompt)
	case "AGENT":
		return r.execAgent(ctx, prompt)
	default:
		return r.execClaude(ctx, prompt)
	}
}

func (r *Runner) execClaude(ctx context.Context, prompt string) (string, error) {
	args := []string{"claude", "-p", prompt}
	if r.cfg.ClaudeModel != "" {
		args = append(args, "--model", r.cfg.ClaudeModel)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("claude execution: %w\noutput: %s", err, string(output))
	}
	return string(output), nil
}

func (r *Runner) execCodex(ctx context.Context, prompt string) (string, error) {
	_ = prompt
	return "", fmt.Errorf("CODEX tool not yet implemented")
}

func (r *Runner) execAgent(ctx context.Context, prompt string) (string, error) {
	args := []string{"agent", "--print", "--output-format", "json", "--trust"}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("agent execution: %w\noutput: %s", err, string(output))
	}
	return string(output), nil
}

func parseOutput(raw string) (*ToolOutput, error) {
	cleaned := stripNonJSON(raw)
	var out ToolOutput
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return nil, fmt.Errorf("json parse: %w\nraw: %s", err, cleaned)
	}
	return &out, nil
}

func stripNonJSON(raw string) string {
	start := strings.Index(raw, "{")
	if start == -1 {
		return raw
	}
	end := strings.LastIndex(raw, "}")
	if end == -1 || end < start {
		return raw
	}
	return raw[start : end+1]
}

func mapActionToOutcome(action string) string {
	switch strings.ToLower(action) {
	case "approve_without_comment":
		return db.ReviewOutcomeApproveWithoutComments
	case "approve_with_comment":
		return db.ReviewOutcomeApproveWithComments
	case "request_changes":
		return db.ReviewOutcomeApproveWithComments
	case "requires_human_review":
		return db.ReviewOutcomeHumanReview
	default:
		return db.ReviewOutcomeToolFailed
	}
}

func (r *Runner) BuildReviewPromptContext(pr db.PullRequest, diff string) string {
	return fmt.Sprintf(`## PR Information:
- **Title:** %s
- **Repository:** %s
- **Author:** %s
- **Branch:** %s

## Diff:
%s
`, pr.Title, pr.Repo, pr.Author, pr.CommitSHA, diff)
}
