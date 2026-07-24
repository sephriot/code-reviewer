package notify

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"text/template"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
)

type TemplateData struct {
	Repo      string
	PRNumber  int
	Title     string
	Author    string
}

type Notifier struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Notifier {
	return &Notifier{cfg: cfg}
}

func (n *Notifier) ReviewStarted(pr db.PullRequest) {
	n.playSound(n.cfg.ReviewStartedSoundEnabled, n.cfg.ReviewStartedSoundFile, pr, "review started")
	n.browserNotify("Review Started", fmt.Sprintf("Review started for PR #%d in %s", pr.PRNumber, pr.Repo))
}

func (n *Notifier) ReviewApproved(pr db.PullRequest) {
	n.playSound(n.cfg.ApprovalSoundEnabled, n.cfg.ApprovalSoundFile, pr, "approved")
	n.browserNotify("PR Approved", fmt.Sprintf("PR #%d in %s by %s approved", pr.PRNumber, pr.Repo, pr.Author))
}

func (n *Notifier) ReviewFailed(pr db.PullRequest, reason string) {
	n.playSound(n.cfg.TimeoutSoundEnabled, n.cfg.TimeoutSoundFile, pr, fmt.Sprintf("review failed: %s", reason))
	n.browserNotify("Review Failed", fmt.Sprintf("PR #%d in %s: %s", pr.PRNumber, pr.Repo, reason))
}

func (n *Notifier) HumanReviewNeeded(pr db.PullRequest) {
	n.playSound(n.cfg.HumanReviewSoundEnabled, n.cfg.HumanReviewSoundFile, pr, "needs human review")
	n.browserNotify("Human Review Needed", fmt.Sprintf("PR #%d in %s by %s needs human review", pr.PRNumber, pr.Repo, pr.Author))
}

func (n *Notifier) PRMergedOrClosed(pr db.PullRequest) {
	n.playSound(n.cfg.MergedOrClosedSoundEnabled, n.cfg.MergedOrClosedSoundFile, pr, "merged or closed")
}

func (n *Notifier) OwnPRReady(pr db.PullRequest) {
	n.playSound(n.cfg.OwnPRReadySoundEnabled, n.cfg.OwnPRReadySoundFile, pr, "your PR is ready")
}

func (n *Notifier) OwnPRNeedsAttention(pr db.PullRequest) {
	n.playSound(n.cfg.OwnPRNeedsAttentionSoundEnabled, n.cfg.OwnPRNeedsAttentionSoundFile, pr, "your PR needs attention")
}

func (n *Notifier) playSound(enabled bool, tmpl string, pr db.PullRequest, fallback string) {
	if !enabled || tmpl == "" {
		return
	}

	msg := renderTemplate(tmpl, pr)
	if msg == "" {
		msg = fallback
	}

	if strings.HasPrefix(tmpl, "say:") {
		n.say(msg)
		return
	}

	if strings.HasPrefix(tmpl, "espeak:") {
		n.say(strings.TrimPrefix(msg, "espeak:"))
		return
	}

	cmd := exec.Command("afplay", tmpl)
	if err := cmd.Start(); err != nil {
		log.Printf("notify: afplay error: %v", err)
	}
}

func (n *Notifier) say(text string) {
	text = strings.TrimPrefix(text, "say:")
	cmd := exec.Command("say", "-r", fmt.Sprintf("%d", n.cfg.SpeechRate), text)
	if err := cmd.Start(); err != nil {
		log.Printf("notify: say error: %v", err)
	}
}

func (n *Notifier) browserNotify(title, body string) {
	log.Printf("notify: [browser] %s: %s", title, body)
}

func renderTemplate(tmpl string, pr db.PullRequest) string {
	data := TemplateData{
		Repo:     pr.Repo,
		PRNumber: pr.PRNumber,
		Title:    pr.Title,
		Author:   pr.Author,
	}

	var buf strings.Builder
	t, err := template.New("notify").Parse(tmpl)
	if err != nil {
		return tmpl
	}
	if err := t.Execute(&buf, data); err != nil {
		return tmpl
	}
	return buf.String()
}
