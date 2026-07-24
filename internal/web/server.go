package web

import (
	"context"
	"embed"
	"encoding/json"
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
	gh "github.com/sephriot/code-reviewer/internal/github"
	"github.com/sephriot/code-reviewer/internal/review"
)

//go:embed templates/*.html static/*
var content embed.FS

type Server struct {
	cfg     *config.Config
	d       *db.DB
	gh      *gh.Client
	runner  *review.Runner

	events    chan review.ReviewEvent
	subs      map[chan review.ReviewEvent]struct{}
	subsMu    sync.RWMutex
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
	mux.HandleFunc("/api/analytics", s.apiAnalytics)

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
		"safe": func(s string) template.HTML { return template.HTML(s) },
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

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	prs, err := s.d.ListOpenPRs()
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

	var comments []db.ReviewComment
	if len(reviews) > 0 {
		comments, err = s.d.ListReviewComments(reviews[0].ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	s.render(w, "pr_detail.html", map[string]interface{}{
		"PR":       pr,
		"Reviews":  reviews,
		"Comments": comments,
	})
}

func (s *Server) historyPage(w http.ResponseWriter, r *http.Request) {
	prs, err := s.d.ListClosedPRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	published, err := s.d.ListPublishedReviews()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.render(w, "history.html", map[string]interface{}{
		"PRs":       prs,
		"Published": published,
	})
}

func (s *Server) analyticsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "analytics.html", nil)
}

func (s *Server) filteredPRs(w http.ResponseWriter, r *http.Request) {
	prs, err := s.d.ListOpenPRs()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	s.render(w, "filtered.html", map[string]interface{}{
		"PRs": prs,
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
	case r.Method == "DELETE":
		http.Error(w, "not implemented", 501)
	default:
		s.respondJSON(w, rv)
	}
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
	var ghComments []gh.ReviewComment
	for _, c := range comments {
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
		err := s.gh.CreateReviewComment(context.Background(), parts[0], parts[1], pr.PRNumber, gh.ReviewComment{
			File:    c.File,
			Line:    c.Line,
			Message: c.Message,
		})
		if err != nil {
			log.Printf("web: failed to post comment: %v", err)
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
	}

	s.respondJSON(w, result)
}
