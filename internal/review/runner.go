package review

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

func (r *Runner) RunReview(ctx context.Context, pr db.PullRequest, promptPath string) (*db.Review, error) {
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		return nil, fmt.Errorf("read prompt file %s: %w", promptPath, err)
	}

	prCtx := BuildPromptContext(pr)
	fullPrompt := prCtx + "\n\n" + string(prompt) + "\n\n" + outputFormatPrompt

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
		PullRequestID:  pr.ID,
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
	args := []string{"claude", "--print"}
	if r.cfg.ClaudeModel != "" {
		args = append(args, "--model", r.cfg.ClaudeModel)
	}
	if r.cfg.ShowThinking {
		args = append(args, "--output-format", "stream-json", "--verbose")
	}
	log.Printf("runner: executing %v", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude start: %w", err)
	}

	var result string
	if r.cfg.ShowThinking {
		result, err = r.readClaudeStreamJSON(stdout)
	} else {
		var out []byte
		out, err = io.ReadAll(io.TeeReader(stdout, os.Stdout))
		if err == nil {
			result = string(out)
		}
	}
	if err != nil {
		cmd.Process.Kill()
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("claude execution: %w\noutput: %s", err, result)
	}
	return result, nil
}

// claudeStreamEvent matches a single --output-format stream-json line.
type claudeStreamEvent struct {
	Type    string `json:"type"`
	Result  string `json:"result,omitempty"`
	Message *struct {
		Content []struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking,omitempty"`
			Text     string `json:"text,omitempty"`
		} `json:"content"`
	} `json:"message,omitempty"`
}

func (r *Runner) readClaudeStreamJSON(stdout io.Reader) (string, error) {
	var resultText string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if ev.Message == nil {
				continue
			}
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "thinking":
					if b.Thinking != "" {
						for _, tl := range strings.Split(b.Thinking, "\n") {
							log.Printf("[THINKING] %s", tl)
						}
					}
				case "text":
					if b.Text != "" {
						os.Stdout.WriteString(b.Text)
					}
				}
			}
		case "result":
			resultText = ev.Result
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("stream read: %w", err)
	}
	return resultText, nil
}

func (r *Runner) execCodex(ctx context.Context, prompt string) (string, error) {
	_ = prompt
	return "", fmt.Errorf("CODEX tool not yet implemented")
}

func (r *Runner) execAgent(ctx context.Context, prompt string) (string, error) {
	args := []string{"agent", "--print", "--output-format", "json", "--trust"}
	log.Printf("runner: executing %v", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("agent start: %w", err)
	}
	output, _ := io.ReadAll(io.TeeReader(stdout, os.Stdout))
	if err := cmd.Wait(); err != nil {
		return "", fmt.Errorf("agent execution: %w\noutput: %s", err, string(output))
	}
	return unwrapAgentOutput(string(output)), nil
}

// Agent wraps --output-format json in {"type":"result","result":"<inner JSON>"}.
// Unwrap extracts inner content so parseOutput can parse it.
func unwrapAgentOutput(raw string) string {
	var env struct {
		Type   string `json:"type"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return raw
	}
	if env.Type != "result" || env.Result == "" {
		return raw
	}
	return env.Result
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

func BuildPromptContext(pr db.PullRequest) string {
	link := fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.PRNumber)
	return fmt.Sprintf("Review pull request: %s\nTitle: %s\nAuthor: %s\nRepository: %s\nPR Number: %d\n", link, pr.Title, pr.Author, pr.Repo, pr.PRNumber)
}
