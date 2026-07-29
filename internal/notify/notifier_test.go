package notify

import (
	"testing"

	"github.com/sephriot/code-reviewer/internal/db"
)

func TestRenderTemplate(t *testing.T) {
	pr := db.PullRequest{
		Title:    "Fix login bug",
		Repo:     "owner/repo",
		Author:   "jane",
		PRNumber: 42,
	}

	tests := []struct {
		name     string
		tmpl     string
		expected string
	}{
		{
			name:     "all placeholders",
			tmpl:     "say:{title} in {repo} by {author} (#{number})",
			expected: "say:Fix login bug in owner/repo by jane (#42)",
		},
		{
			name:     "say with title and repo",
			tmpl:     "say:Review started for PR {title} in {repo}",
			expected: "say:Review started for PR Fix login bug in owner/repo",
		},
		{
			name:     "no placeholders",
			tmpl:     "say:hello world",
			expected: "say:hello world",
		},
		{
			name:     "empty string",
			tmpl:     "",
			expected: "",
		},
		{
			name:     "only title",
			tmpl:     "afplay /path/to/{title}.wav",
			expected: "afplay /path/to/Fix login bug.wav",
		},
		{
			name:     "only braces no content",
			tmpl:     "say:{title} {repo} {author} #{number}",
			expected: "say:Fix login bug owner/repo jane #42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := renderTemplate(tt.tmpl, pr)
			if result != tt.expected {
				t.Errorf("renderTemplate(%q, pr) = %q, want %q", tt.tmpl, result, tt.expected)
			}
		})
	}
}

func TestRenderTemplateEmptyFields(t *testing.T) {
	pr := db.PullRequest{
		Title:    "",
		Repo:     "",
		Author:   "",
		PRNumber: 0,
	}

	result := renderTemplate("say:{title} {repo} {author} #{number}", pr)
	expected := "say:   #0"
	if result != expected {
		t.Errorf("renderTemplate with empty fields = %q, want %q", result, expected)
	}
}

func TestMutedDefaultsFalse(t *testing.T) {
	n := New(nil)
	if n.Muted() {
		t.Fatal("expected muted=false by default")
	}
}

func TestPlaySoundSkippedWhenMuted(t *testing.T) {
	n := New(nil)
	var calls int
	n.soundHook = func(action, text string) {
		calls++
	}
	n.SetMuted(true)
	n.playSound(true, "say:hello", db.PullRequest{Title: "t"}, "fallback")
	if calls != 0 {
		t.Fatalf("muted playSound called soundHook %d times, want 0", calls)
	}
}

func TestPlaySoundRunsWhenUnmuted(t *testing.T) {
	n := New(nil)
	var gotAction, gotText string
	n.soundHook = func(action, text string) {
		gotAction = action
		gotText = text
	}
	n.playSound(true, "say:hello {title}", db.PullRequest{Title: "PR"}, "fallback")
	if gotAction != "say" {
		t.Fatalf("action=%q want say", gotAction)
	}
	if gotText != "hello PR" {
		t.Fatalf("text=%q want %q", gotText, "hello PR")
	}
}
