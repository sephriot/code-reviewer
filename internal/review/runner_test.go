package review

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
)

func TestReadClaudeStreamJSON(t *testing.T) {
	oldWriter := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	input := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"checking"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"{\"action\":\"approve_without_comment\"}"}}}`,
		`{"type":"result","result":"{\"action\":\"approve_without_comment\"}"}`,
	}, "\n")

	got, err := (&Runner{}).readClaudeStreamJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("readClaudeStreamJSON() error = %v", err)
	}
	if want := `{"action":"approve_without_comment"}`; got != want {
		t.Fatalf("readClaudeStreamJSON() = %q, want %q", got, want)
	}
}

func TestReadAgentStreamJSON(t *testing.T) {
	oldWriter := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"reviewing"}]}}`,
		`{"type":"result","result":"{\"action\":\"requires_human_review\"}"}`,
	}, "\n")

	got, err := readAgentStreamJSON(strings.NewReader(input))
	if err != nil {
		t.Fatalf("readAgentStreamJSON() error = %v", err)
	}
	if want := `{"action":"requires_human_review"}`; got != want {
		t.Fatalf("readAgentStreamJSON() = %q, want %q", got, want)
	}
}

func TestUnwrapAgentOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "result envelope",
			input: `{"type":"result","result":"{\"action\":\"approve_without_comment\"}"}`,
			want:  `{"action":"approve_without_comment"}`,
		},
		{
			name:  "plain output",
			input: `{"action":"approve_without_comment"}`,
			want:  `{"action":"approve_without_comment"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unwrapAgentOutput(tt.input); got != tt.want {
				t.Fatalf("unwrapAgentOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		want []string
	}{
		{
			name: "default headless command",
			cfg:  &config.Config{},
			want: []string{"agent", "--print", "--output-format", "json", "--trust"},
		},
		{
			name: "streaming command omits partial output",
			cfg:  &config.Config{ShowThinking: true},
			want: []string{"agent", "--print", "--output-format", "stream-json", "--trust"},
		},
		{
			name: "custom permissions are preserved",
			cfg: &config.Config{
				ReviewAgentArgv: []string{"agent", "--print", "--output-format", "json", "--trust", "--force"},
			},
			want: []string{"agent", "--print", "--output-format", "json", "--trust", "--force"},
		},
		{
			name: "custom permissions work with streaming",
			cfg: &config.Config{
				ShowThinking:    true,
				ReviewAgentArgv: []string{"agent", "--print", "--output-format", "json", "--trust", "--yolo", "--stream-partial-output"},
			},
			want: []string{"agent", "--print", "--output-format", "stream-json", "--trust", "--yolo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (&Runner{cfg: tt.cfg}).agentArgs()
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("agentArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIsAgentToolEvent(t *testing.T) {
	for _, eventType := range []string{"tool_call", "tool_result", "tool_use", "tool_use_result"} {
		if !isAgentToolEvent(eventType) {
			t.Errorf("isAgentToolEvent(%q) = false, want true", eventType)
		}
	}
	if isAgentToolEvent("assistant") {
		t.Error("isAgentToolEvent(assistant) = true, want false")
	}
}

func TestReadAgentStreamJSONKeepsAssistantOutputAndHidesToolCalls(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"review output"}]}}`,
		`{"type":"tool_call","tool_call":{"name":"Shell","command":"bash secret-command"}}`,
		`{"type":"result","result":"{\"action\":\"approve_without_comment\"}"}`,
	}, "\n")

	if _, err := readAgentStreamJSON(strings.NewReader(input)); err != nil {
		t.Fatalf("readAgentStreamJSON() error = %v", err)
	}
	if !strings.Contains(logs.String(), "review output") {
		t.Fatalf("assistant output missing from logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret-command") {
		t.Fatalf("tool call leaked into logs: %s", logs.String())
	}
}
