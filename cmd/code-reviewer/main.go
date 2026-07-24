package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sephriot/code-reviewer/internal/config"
	"github.com/sephriot/code-reviewer/internal/db"
	gh "github.com/sephriot/code-reviewer/internal/github"
	"github.com/sephriot/code-reviewer/internal/notify"
	"github.com/sephriot/code-reviewer/internal/review"
	"github.com/sephriot/code-reviewer/internal/scanner"
	"github.com/sephriot/code-reviewer/internal/web"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down...")
		cancel()
	}()

	if err := run(ctx); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log.Printf("starting code-reviewer (log=%s, interval=%v, timeout=%v)", cfg.LogLevel, cfg.PollInterval, cfg.ReviewTimeout)

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer database.Close()

	ghClient := gh.New(cfg.GithubToken, cfg.GithubUsername)
	runner := review.NewRunner(cfg)
	notifier := notify.New(cfg)

	eventBridge := func(event review.ReviewEvent) {
		pr := db.PullRequest{ID: event.PR.ID, Repo: event.PR.Repo, PRNumber: event.PR.PRNumber, Title: event.PR.Title, Author: event.PR.Author}
		switch event.Type {
		case review.EventReviewStart:
			notifier.ReviewStarted(pr)
		case review.EventReviewSuccess:
			notifier.ReviewApproved(pr)
		case review.EventReviewFail:
			notifier.ReviewFailed(pr, event.Message)
		case review.EventHumanReviewNeeded:
			notifier.HumanReviewNeeded(pr)
		}
	}

	webServer := web.New(cfg, database, ghClient, runner)

	eventBridge2 := func(event review.ReviewEvent) {
		eventBridge(event)
		webServer.OnEvent(event)
	}

	reactor := review.NewReactor(cfg, database, ghClient, runner, eventBridge2)
	sc := scanner.New(cfg, ghClient, database, func() {
		go func() {
			if err := reactor.ProcessQueue(ctx); err != nil {
				log.Printf("reactor: queue error: %v", err)
			}
		}()
	})

	scanTicker := time.NewTicker(cfg.PollInterval)
	defer scanTicker.Stop()

	reactorTicker := time.NewTicker(1 * time.Minute)
	defer reactorTicker.Stop()

	go func() {
		if err := sc.Scan(ctx); err != nil {
			log.Printf("scan: initial scan error: %v", err)
		}
	}()

	go func() {
		if err := reactor.ProcessQueue(ctx); err != nil {
			log.Printf("reactor: initial queue error: %v", err)
		}
	}()

	if cfg.WebEnabled {
		go func() {
			if err := webServer.Serve(ctx); err != nil {
				log.Printf("web: server error: %v", err)
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-scanTicker.C:
			go func() {
				if err := sc.Scan(ctx); err != nil {
					log.Printf("scan: error: %v", err)
				}
			}()
		case <-reactorTicker.C:
			go func() {
				if err := reactor.ProcessQueue(ctx); err != nil {
					log.Printf("reactor: queue error: %v", err)
				}
			}()
		}
	}
}
