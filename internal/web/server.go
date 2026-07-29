package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	"github.com/sephriot/code-reviewer/internal/notify"
	gh "github.com/sephriot/code-reviewer/internal/github"
	"github.com/sephriot/code-reviewer/internal/review"
)

//go:embed templates/*.html static/*
var content embed.FS

type QueueCanceller interface {
	CancelRequest(id int64) error
}

type Server struct {
	cfg    *config.Config
	d      *db.DB
	gh     *gh.Client
	runner *review.Runner

	canceller QueueCanceller
	notifier  *notify.Notifier

	events chan review.ReviewEvent
	subs   map[chan review.ReviewEvent]struct{}
	subsMu sync.RWMutex
}

func New(cfg *config.Config, d *db.DB, gh *gh.Client, runner *review.Runner) *Server {
	s := &Server{
		cfg:    cfg,
		d:      d,
		gh:     gh,
		runner: runner,
		events: make(chan review.ReviewEvent, 100),
		subs:   make(map[chan review.ReviewEvent]struct{}),
	}
	go s.broadcastLoop()
	return s
}

func (s *Server) SetQueueCanceller(c QueueCanceller) {
	s.canceller = c
}

func (s *Server) SetNotifier(n *notify.Notifier) {
	s.notifier = n
}

func (s *Server) OnEvent(event review.ReviewEvent) {
	s.events <- event
}

func (s *Server) Subscribe() chan review.ReviewEvent {
	ch := make(chan review.ReviewEvent, 10)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch
}

func (s *Server) Unsubscribe(ch chan review.ReviewEvent) {
	s.subsMu.Lock()
	delete(s.subs, ch)
	s.subsMu.Unlock()
	close(ch)
}

func (s *Server) broadcastLoop() {
	for event := range s.events {
		s.subsMu.RLock()
		for ch := range s.subs {
			select {
			case ch <- event:
			default:
			}
		}
		s.subsMu.RUnlock()
	}
}

func (s *Server) Serve(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.dashboard)
	mux.HandleFunc("/pr/", s.prDetail)
	mux.HandleFunc("/analytics", s.analyticsPage)
	mux.HandleFunc("/history", s.historyPage)
	mux.HandleFunc("/filtered", s.filteredPRs)
	mux.HandleFunc("/events", s.sseHandler)

	mux.HandleFunc("/api/pr/", s.apiPR)
	mux.HandleFunc("/api/review/", s.apiReview)
	mux.HandleFunc("/api/review-request/", s.apiReviewRequest)
	mux.HandleFunc("/api/inline-comment/", s.apiInlineComment)
	mux.HandleFunc("/api/snippet", s.apiSnippet)
	mux.HandleFunc("/api/analytics", s.apiAnalytics)
	mux.HandleFunc("/api/notifications/mute", s.apiMuteNotifications)

	staticFS, _ := fs.Sub(content, "static")
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	addr := fmt.Sprintf("%s:%d", s.cfg.WebHost, s.cfg.WebPort)
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	return srv.ListenAndServe()
}

func (s *Server) render(w http.ResponseWriter, name string, data interface{}) {
	t := template.New("")
	t.Funcs(template.FuncMap{
		"safe":       func(s string) template.HTML { return template.HTML(s) },
		"formatTime": func(t time.Time) string { return t.Format("2006-01-02 15:04") },
	})
	t, err := t.ParseFS(content, "templates/base.html", "templates/"+name)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := t.ExecuteTemplate(w, "base", data); err != nil {
		log.Printf("web: template error: %v", err)
	}
}

func (s *Server) respondJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func (s *Server) apiMuteNotifications(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		http.Error(w, "mute unavailable", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.respondJSON(w, map[string]bool{"muted": s.notifier.Muted()})
	case http.MethodPost:
		var body struct {
			Muted *bool `json:"muted"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Muted == nil {
			http.Error(w, "invalid json: muted bool required", http.StatusBadRequest)
			return
		}
		s.notifier.SetMuted(*body.Muted)
		s.respondJSON(w, map[string]bool{"muted": s.notifier.Muted()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	prs, err := s.d.ListPRsNeedingReview()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	requests, err := s.d.ListReviewRequests()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	prMap := make(map[int64]db.PullRequest)
	outcomeMap := make(map[int64]string)
	for _, pr := range prs {
		prMap[pr.ID] = pr
		latest, err := s.d.GetLatestReviewByPR(pr.ID)
		if err == nil && latest != nil {
			outcomeMap[pr.ID] = latest.Outcome
		}
	}
	// Queue can include filtered/history PRs that are absent from ListPRsNeedingReview.
	for _, rr := range requests {
		if _, ok := prMap[rr.PullRequestID]; ok {
			continue
		}
		pr, err := s.d.GetPR(rr.PullRequestID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if pr != nil {
			prMap[pr.ID] = *pr
		}
	}
	s.render(w, "dashboard.html", map[string]interface{}{
		"PRs":        prs,
		"Requests":   requests,
		"PRMap":      prMap,
		"OutcomeMap": outcomeMap,
	})
}

func (s *Server) prDetail(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/pr/")
	if idStr == "" {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	pr, err := s.d.GetPR(id)
	if err != nil || pr == nil {
		http.NotFound(w, r)
		return
	}

	reviews, err := s.d.ListReviewsForPR(pr.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	type reviewWithComments struct {
		Review   db.Review
		Comments []db.ReviewComment
	}

	var reviewList []reviewWithComments
	nonPublished := 0
	for _, r := range reviews {
		if r.Published {
			continue
		}
		comments, _ := s.d.ListReviewComments(r.ID)
		reviewList = append(reviewList, reviewWithComments{Review: r, Comments: comments})
		nonPublished++
	}

	var latestOutcome string
	if latest, err := s.d.GetLatestReviewByPR(pr.ID); err == nil && latest != nil {
		latestOutcome = latest.Outcome
	}

	s.render(w, "pr_detail.html", map[string]interface{}{
		"PR":            pr,
		"Reviews":       reviewList,
		"LatestOutcome": latestOutcome,
	})
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) {
	prs, err := s.d.ListHistoryPRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	published, err := s.d.ListPublishedReviews()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	known := make(map[int64]struct{}, len(prs))
	for _, pr := range prs {
		known[pr.ID] = struct{}{}
	}
	for _, rev := range published {
		if _, ok := known[rev.PullRequestID]; ok {
			continue
		}
		pr, err := s.d.GetPR(rev.PullRequestID)
		if err != nil || pr == nil {
			continue
		}
		prs = append(prs, *pr)
		known[pr.ID] = struct{}{}
	}

	feed := BuildHistoryFeed(prs, published)
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			page = n
		}
	}
	items, meta := PaginateFeed(feed, page, 10)
	s.render(w, "history.html", map[string]interface{}{
		"Items": items,
		"Meta":  meta,
	})
}

func (s *Server) analyticsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "analytics.html", nil)
}

func (s *Server) filteredPRs(w http.ResponseWriter, r *http.Request) {
	prs, err := s.d.ListFilteredPRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	outcomeMap := make(map[int64]string)
	for _, pr := range prs {
		latest, err := s.d.GetLatestReviewByPR(pr.ID)
		if err == nil && latest != nil {
			outcomeMap[pr.ID] = latest.Outcome
		}
	}

	s.render(w, "filtered.html", map[string]interface{}{
		"PRs":        prs,
		"OutcomeMap": outcomeMap,
	})
}

func (s *Server) sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := s.Subscribe()
	defer s.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (s *Server) apiPR(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/pr/"), "/")
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}

	prID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) >= 2 && parts[1] == "review" && r.Method == "POST":
		s.requestReview(w, r, prID)
	default:
		s.getPR(w, prID)
	}
}

func (s *Server) getPR(w http.ResponseWriter, prID int64) {
	pr, err := s.d.GetPR(prID)
	if err != nil || pr == nil {
		http.Error(w, "not found", 404)
		return
	}
	reviews, _ := s.d.ListReviewsForPR(prID)
	comments := []db.ReviewComment{}
	if len(reviews) > 0 {
		comments, _ = s.d.ListReviewComments(reviews[0].ID)
	}
	s.respondJSON(w, map[string]interface{}{
		"pr":       pr,
		"reviews":  reviews,
		"comments": comments,
	})
}

func (s *Server) requestReview(w http.ResponseWriter, r *http.Request, prID int64) {
	rrID, err := s.d.CreateReviewRequest(prID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.respondJSON(w, map[string]interface{}{"review_request_id": rrID})
}

func (s *Server) apiReviewRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/review-request/")
	idStr = strings.Trim(idStr, "/")
	rrID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || rrID <= 0 {
		http.NotFound(w, r)
		return
	}
	if s.canceller == nil {
		log.Printf("web: cancel request %d: canceller unavailable", rrID)
		http.Error(w, "queue cancel unavailable", http.StatusServiceUnavailable)
		return
	}
	log.Printf("web: DELETE /api/review-request/%d", rrID)
	if err := s.canceller.CancelRequest(rrID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			log.Printf("web: cancel request %d: not found", rrID)
			http.NotFound(w, r)
			return
		}
		log.Printf("web: cancel request %d failed: %v", rrID, err)
		http.Error(w, err.Error(), 500)
		return
	}
	log.Printf("web: cancel request %d ok", rrID)
	s.respondJSON(w, map[string]string{"status": "removed"})
}

func (s *Server) apiReview(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/review/"), "/")
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}

	reviewID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	rv, err := s.d.GetReview(reviewID)
	if err != nil || rv == nil {
		http.Error(w, "not found", 404)
		return
	}

	switch {
	case len(parts) >= 2 && parts[1] == "publish" && r.Method == "POST":
		s.publishReview(w, rv)
	case len(parts) >= 2 && parts[1] == "publish-comments" && r.Method == "POST":
		s.publishReviewComments(w, rv)
	case len(parts) >= 2 && parts[1] == "general-comment" && r.Method == "PUT":
		var body struct {
			Comment string `json:"comment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if err := s.d.UpdateReviewGeneralComment(reviewID, body.Comment); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.respondJSON(w, map[string]string{"status": "updated"})
	case r.Method == "DELETE":
		http.Error(w, "not implemented", 501)
	default:
		s.respondJSON(w, rv)
	}
}

func (s *Server) apiInlineComment(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/inline-comment/"), "/")
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if len(parts) >= 2 && parts[1] == "publish" {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		s.publishInlineComment(w, id)
		return
	}

	switch r.Method {
	case "DELETE":
		if err := s.d.DeleteReviewComment(id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.respondJSON(w, map[string]string{"status": "deleted"})
	case "PUT":
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if err := s.d.UpdateReviewComment(id, body.Message); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.respondJSON(w, map[string]string{"status": "updated"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (s *Server) publishInlineComment(w http.ResponseWriter, commentID int64) {
	c, err := s.d.GetReviewComment(commentID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if c == nil {
		http.Error(w, "comment not found", 404)
		return
	}
	if c.Published {
		s.respondJSON(w, map[string]string{"status": "already_published"})
		return
	}

	rv, err := s.d.GetReview(c.ReviewID)
	if err != nil || rv == nil {
		http.Error(w, "review not found", 404)
		return
	}
	pr, err := s.d.GetPR(rv.PullRequestID)
	if err != nil || pr == nil {
		http.Error(w, "PR not found", 404)
		return
	}
	parts := strings.Split(pr.Repo, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid repo", 500)
		return
	}

	commitID := rv.CommitSHA
	if commitID == "" {
		commitID = pr.CommitSHA
	}
	if err := s.gh.CreateReviewComment(context.Background(), parts[0], parts[1], pr.PRNumber, gh.ReviewComment{
		File:     c.File,
		Line:     c.Line,
		Message:  c.Message,
		CommitID: commitID,
	}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := s.d.PublishReviewComment(c.ID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.respondJSON(w, map[string]string{"status": "published"})
}

func (s *Server) publishReview(w http.ResponseWriter, rv *db.Review) {
	pr, err := s.d.GetPR(rv.PullRequestID)
	if err != nil || pr == nil {
		http.Error(w, "PR not found", 404)
		return
	}

	parts := strings.Split(pr.Repo, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid repo", 500)
		return
	}

	comments, _ := s.d.ListReviewComments(rv.ID)
	var unpublished []db.ReviewComment
	var ghComments []gh.ReviewComment
	for _, c := range comments {
		if c.Published {
			continue
		}
		unpublished = append(unpublished, c)
		ghComments = append(ghComments, gh.ReviewComment{
			File:    c.File,
			Line:    c.Line,
			Message: c.Message,
		})
	}

	err = s.gh.SubmitReview(context.Background(), parts[0], parts[1], pr.PRNumber, gh.ReviewSubmission{
		Outcome:  rv.Outcome,
		Body:     rv.GeneralComment,
		Comments: ghComments,
	})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	for _, c := range unpublished {
		if err := s.d.PublishReviewComment(c.ID); err != nil {
			log.Printf("web: failed to mark comment %d published: %v", c.ID, err)
		}
	}
	s.d.PublishReview(rv.ID)
	s.respondJSON(w, map[string]string{"status": "published"})
}

func (s *Server) publishReviewComments(w http.ResponseWriter, rv *db.Review) {
	pr, err := s.d.GetPR(rv.PullRequestID)
	if err != nil || pr == nil {
		http.Error(w, "PR not found", 404)
		return
	}

	parts := strings.Split(pr.Repo, "/")
	if len(parts) < 2 {
		http.Error(w, "invalid repo", 500)
		return
	}

	comments, _ := s.d.ListReviewComments(rv.ID)
	for _, c := range comments {
		if c.Published {
			continue
		}
		commitID := rv.CommitSHA
		if commitID == "" {
			commitID = pr.CommitSHA
		}
		err := s.gh.CreateReviewComment(context.Background(), parts[0], parts[1], pr.PRNumber, gh.ReviewComment{
			File:     c.File,
			Line:     c.Line,
			Message:  c.Message,
			CommitID: commitID,
		})
		if err != nil {
			log.Printf("web: failed to post comment: %v", err)
			continue
		}
		if err := s.d.PublishReviewComment(c.ID); err != nil {
			log.Printf("web: failed to mark comment %d published: %v", c.ID, err)
		}
	}
	s.respondJSON(w, map[string]string{"status": "comments_published"})
}

func (s *Server) apiAnalytics(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	groupBy := r.URL.Query().Get("group")

	days := 30
	switch period {
	case "7d":
		days = 7
	case "30d":
		days = 30
	case "quarter":
		days = 90
	case "year":
		days = 365
	case "all":
		days = 365 * 10
	}
	since := time.Now().AddDate(0, 0, -days)

	result := map[string]interface{}{
		"period": period,
		"group":  groupBy,
		"since":  since,
	}

	if groupBy == "outcome" || groupBy == "" {
		outcomes := []string{
			db.ReviewOutcomeApproveWithoutComments,
			db.ReviewOutcomeApproveWithComments,
			db.ReviewOutcomeChangesRequested,
			db.ReviewOutcomeHumanReview,
			db.ReviewOutcomeToolFailed,
			db.ReviewOutcomeReviewedExternally,
		}
		counts := map[string]int{}
		for _, o := range outcomes {
			count, _ := s.d.CountReviewsByOutcomeSince(o, since)
			counts[o] = count
		}
		total, _ := s.d.CountReviewsSince(since)
		published, _ := s.d.CountPublishedReviewsSince(since)
		result["data"] = counts
		result["total"] = total
		result["published"] = published

		bucket := db.TrendBucketDay
		switch period {
		case "quarter", "year", "all":
			bucket = db.TrendBucketWeek
		}
		rows, err := s.d.ReviewsByOutcomeOverTime(since, bucket)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		result["trends"] = db.FillTrendBuckets(since, time.Now(), bucket, rows)

		authors, err := s.d.ReviewsByAuthorStats(since, 15)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		result["authors"] = authors
	}

	s.respondJSON(w, result)
}

func (s *Server) apiSnippet(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	sha := r.URL.Query().Get("sha")
	file := r.URL.Query().Get("file")
	lineStr := r.URL.Query().Get("line")

	if repo == "" || sha == "" || file == "" || lineStr == "" {
		http.Error(w, "missing params", 400)
		return
	}

	line, err := strconv.Atoi(lineStr)
	if err != nil {
		http.Error(w, "invalid line", 400)
		return
	}

	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		http.Error(w, "invalid repo", 400)
		return
	}

	content, startLine, err := s.gh.GetFileContent(r.Context(), parts[0], parts[1], sha, file, line, 5)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	s.respondJSON(w, map[string]any{
		"content":     content,
		"start_line":  startLine,
		"target_line": line,
	})
}
