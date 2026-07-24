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
	"time"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

type ToolOutput struct {
	Action   string          `json:"action"`
	Comment  string          `json:"comment"`
	Summary  string          `json:"summary"`
	Reason   string          `json:"reason"`
	Comments []ToolComment   `json:"comments"`
}

type ToolComment struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type ReviewResult struct {
	Review   *db.Review
	Comments []ToolComment
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

func (r *Runner) RunReview(ctx context.Context, pr db.PullRequest, promptPath string) (*ReviewResult, error) {
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

	return &ReviewResult{Review: review, Comments: toolOut.Comments}, nil
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
		args = append(args, "--output-format", "stream-json", "--include-partial-messages", "--verbose")
	}
	log.Printf("runner: executing %v", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Stderr = log.Writer()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude start: %w", err)
	}
	startedAt := time.Now()
	log.Printf("runner: claude started (pid=%d)", cmd.Process.Pid)
	emitRunnerStatus("claude", "started (pid=%d)", cmd.Process.Pid)

	if r.cfg.ShowThinking {
		result, err := r.readClaudeStreamJSON(stdout)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			log.Printf("runner: claude stream failed after %s: %v", time.Since(startedAt).Round(time.Millisecond), err)
			emitRunnerStatus("claude", "failed: %v", err)
			return "", err
		}
		if err := cmd.Wait(); err != nil {
			log.Printf("runner: claude failed after %s: %v", time.Since(startedAt).Round(time.Millisecond), err)
			emitRunnerStatus("claude", "failed: %v", err)
			return "", fmt.Errorf("claude execution: %w\noutput: %s", err, result)
		}
		log.Printf("runner: claude completed in %s (result_bytes=%d)", time.Since(startedAt).Round(time.Millisecond), len(result))
		emitRunnerStatus("claude", "completed in %s (result_bytes=%d)", time.Since(startedAt).Round(time.Millisecond), len(result))
		return result, nil
	}

	var buf strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		logRunnerOutput("claude", line)
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Printf("runner: claude stdout failed after %s: %v", time.Since(startedAt).Round(time.Millisecond), err)
		emitRunnerStatus("claude", "stdout failed: %v", err)
		return "", fmt.Errorf("claude stdout read: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("runner: claude failed after %s: %v", time.Since(startedAt).Round(time.Millisecond), err)
		emitRunnerStatus("claude", "failed: %v", err)
		return "", fmt.Errorf("claude execution: %w\noutput: %s", err, buf.String())
	}
	log.Printf("runner: claude completed in %s (output_bytes=%d)", time.Since(startedAt).Round(time.Millisecond), buf.Len())
	emitRunnerStatus("claude", "completed in %s (output_bytes=%d)", time.Since(startedAt).Round(time.Millisecond), buf.Len())
	return buf.String(), nil
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
	Event *struct {
		Type  string `json:"type"`
		Delta *struct {
			Type     string `json:"type"`
			Text     string `json:"text,omitempty"`
			Thinking string `json:"thinking,omitempty"`
		} `json:"delta,omitempty"`
	} `json:"event,omitempty"`
}

func (r *Runner) readClaudeStreamJSON(stdout io.Reader) (string, error) {
	var resultText string
	emittedText := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		log.Printf("runner: claude stream: %s", line)
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
					if b.Text != "" && !emittedText {
						logRunnerOutput("claude", b.Text)
						emittedText = true
					}
				}
			}
		case "stream_event":
			if ev.Event == nil || ev.Event.Delta == nil {
				continue
			}
			switch ev.Event.Delta.Type {
			case "thinking_delta":
				if ev.Event.Delta.Thinking != "" {
					log.Printf("runner: claude thinking: %s", ev.Event.Delta.Thinking)
				}
			case "text_delta":
				if ev.Event.Delta.Text != "" {
					logRunnerOutput("claude", ev.Event.Delta.Text)
					emittedText = true
				}
			}
		case "result":
			resultText = ev.Result
			log.Printf("runner: claude result received (result_bytes=%d)", len(resultText))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("stream read: %w", err)
	}
	if resultText == "" {
		return "", fmt.Errorf("stream ended without a result event")
	}
	if !emittedText {
		logRunnerOutput("claude", resultText)
	}
	return resultText, nil
}

func (r *Runner) execCodex(ctx context.Context, prompt string) (string, error) {
	_ = prompt
	return "", fmt.Errorf("CODEX tool not yet implemented")
}

func (r *Runner) agentArgs() []string {
	args := []string{"agent", "--print", "--output-format", "json", "--trust"}
	if len(r.cfg.ReviewAgentArgv) > 0 {
		args = append([]string(nil), r.cfg.ReviewAgentArgv...)
	}
	if r.cfg.ShowThinking {
		args = setOutputFormat(args, "stream-json")
		args = removeArg(args, "--stream-partial-output")
	}
	return args
}

func setOutputFormat(args []string, format string) []string {
	for i, arg := range args {
		if arg == "--output-format" {
			if i+1 < len(args) {
				args[i+1] = format
			}
			return args
		}
		if strings.HasPrefix(arg, "--output-format=") {
			args[i] = "--output-format=" + format
			return args
		}
	}
	return append(args, "--output-format", format)
}

func removeArg(args []string, target string) []string {
	filtered := args[:0]
	for _, arg := range args {
		if arg != target {
			filtered = append(filtered, arg)
		}
	}
	return filtered
}

func (r *Runner) execAgent(ctx context.Context, prompt string) (string, error) {
	args := r.agentArgs()
	log.Printf("runner: executing %v", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = log.Writer()

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("agent start: %w", err)
	}
	startedAt := time.Now()
	log.Printf("runner: agent started (pid=%d)", cmd.Process.Pid)
	emitRunnerStatus("agent", "started (pid=%d)", cmd.Process.Pid)
	result, err := readAgentStreamJSON(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Printf("runner: agent stream failed after %s: %v", time.Since(startedAt).Round(time.Millisecond), err)
		emitRunnerStatus("agent", "failed: %v", err)
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		log.Printf("runner: agent failed after %s: %v", time.Since(startedAt).Round(time.Millisecond), err)
		emitRunnerStatus("agent", "failed: %v", err)
		return "", fmt.Errorf("agent execution: %w\noutput: %s", err, result)
	}
	log.Printf("runner: agent completed in %s (result_bytes=%d)", time.Since(startedAt).Round(time.Millisecond), len(result))
	emitRunnerStatus("agent", "completed in %s (result_bytes=%d)", time.Since(startedAt).Round(time.Millisecond), len(result))
	return unwrapAgentOutput(result), nil
}

func readAgentStreamJSON(stdout io.Reader) (string, error) {
	var resultText string
	var raw strings.Builder
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		raw.WriteString(line)
		raw.WriteByte('\n')

		var ev claudeStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			log.Printf("runner: agent emitted non-JSON output (%d bytes)", len(line))
			continue
		}
		if isAgentToolEvent(ev.Type) {
			continue
		}
		log.Printf("runner: agent stream: %s", line)
		if ev.Type == "result" {
			resultText = ev.Result
			log.Printf("runner: agent result received (result_bytes=%d)", len(resultText))
			continue
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("stream read: %w", err)
	}
	if resultText != "" {
		return resultText, nil
	}
	if raw.Len() == 0 {
		return "", fmt.Errorf("stream ended without output")
	}
	return raw.String(), nil
}

func isAgentToolEvent(eventType string) bool {
	switch eventType {
	case "tool_call", "tool_result", "tool_use", "tool_use_result":
		return true
	default:
		return false
	}
}

func logRunnerOutput(tool, output string) {
	if output == "" {
		return
	}
	log.Printf("runner: %s output: %s", tool, output)
}

func emitRunnerStatus(tool, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(os.Stdout, "runner: %s %s\n", tool, message)
}

// Agent's stream-json result event wraps the review JSON in
// {"type":"result","result":"<inner JSON>"}.
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
		return db.ReviewOutcomeChangesRequested
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
