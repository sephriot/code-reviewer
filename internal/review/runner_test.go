package review

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

func TestMapActionToOutcomePreservesRequestChanges(t *testing.T) {
	if got := mapActionToOutcome("request_changes"); got != db.ReviewOutcomeChangesRequested {
		t.Fatalf("outcome = %q, want %q", got, db.ReviewOutcomeChangesRequested)
	}
}

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

func TestReadAgentStreamJSONErrorsWithoutResult(t *testing.T) {
	oldWriter := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"halfway through"}]}}`,
		`{"type":"connection","subtype":"reconnecting","attempt":2,"endpoint_url":"https://agentn.example/"}`,
		`{"type":"retry","subtype":"starting","attempt":2,"is_resume":true}`,
		`{"type":"connection","subtype":"reconnected"}`,
	}, "\n")

	got, err := readAgentStreamJSON(strings.NewReader(input))
	if err == nil {
		t.Fatalf("readAgentStreamJSON() error = nil, got %q", got)
	}
	msg := err.Error()
	if strings.Contains(msg, `"type":"connection"`) || strings.Contains(msg, "endpoint_url") {
		t.Fatalf("error embeds raw stream dump: %s", msg)
	}
	if !strings.Contains(msg, "stream ended without a result event") {
		t.Fatalf("error missing missing-result cause: %s", msg)
	}
	if !strings.Contains(msg, "2 reconnect") {
		t.Fatalf("error missing reconnect count: %s", msg)
	}
	if !strings.Contains(msg, "halfway through") {
		t.Fatalf("error missing last progress: %s", msg)
	}
	if len(msg) > 500 {
		t.Fatalf("error too long (%d bytes): %s", len(msg), msg)
	}
}

func TestReadAgentStreamJSONErrorsWithoutResultNoReconnect(t *testing.T) {
	oldWriter := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(oldWriter) })

	input := `{"type":"assistant","message":{"content":[{"type":"text","text":"started"}]}}`
	_, err := readAgentStreamJSON(strings.NewReader(input))
	if err == nil {
		t.Fatal("readAgentStreamJSON() error = nil, want error")
	}
	msg := err.Error()
	if msg != "stream ended without a result event. Last progress: started" {
		t.Fatalf("readAgentStreamJSON() error = %q", msg)
	}
}

func TestFormatAgentExecutionFailureKeepsShortOutput(t *testing.T) {
	err := formatAgentExecutionFailure(fmt.Errorf("exit status 1"), `{"action":"approve_without_comment"}`)
	want := "agent execution: exit status 1\noutput: {\"action\":\"approve_without_comment\"}"
	if err.Error() != want {
		t.Fatalf("formatAgentExecutionFailure() = %q, want %q", err.Error(), want)
	}
}

func TestFormatAgentExecutionFailureSummarizesStreamDump(t *testing.T) {
	dump := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Verified the race against reconcileWorkerState"}]}}`,
		`{"type":"connection","subtype":"reconnecting","attempt":3,"endpoint_url":"https://agentn.global.api5.cursor.sh"}`,
		`{"type":"retry","subtype":"resuming","attempt":3}`,
	}, "\n")
	// Make it clearly large like production dumps.
	dump = dump + "\n" + strings.Repeat(`{"type":"thinking","subtype":"delta","text":"x"}`+"\n", 50)

	err := formatAgentExecutionFailure(fmt.Errorf("exit status 1"), dump)
	msg := err.Error()
	if strings.Contains(msg, `"type":"thinking"`) || strings.Contains(msg, "endpoint_url") {
		t.Fatalf("error embeds raw stream dump: %s", msg)
	}
	if !strings.Contains(msg, "agent execution: exit status 1") {
		t.Fatalf("missing exec prefix: %s", msg)
	}
	if !strings.Contains(msg, "3 reconnect") {
		t.Fatalf("missing reconnect summary: %s", msg)
	}
	if !strings.Contains(msg, "Verified the race") {
		t.Fatalf("missing last progress: %s", msg)
	}
	if len(msg) > 500 {
		t.Fatalf("error too long (%d bytes): %s", len(msg), msg)
	}
}
