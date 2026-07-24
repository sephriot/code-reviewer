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

	log.Printf("scan: next run in %v", cfg.PollInterval)
	scanTicker := time.NewTicker(cfg.PollInterval)
	defer scanTicker.Stop()

	reactorInterval := 1 * time.Minute
	log.Printf("reactor: next run in %v", reactorInterval)
	reactorTicker := time.NewTicker(reactorInterval)
	defer reactorTicker.Stop()

	log.Println("scan: initial run starting")
	go func() {
		if err := sc.Scan(ctx); err != nil {
			log.Printf("scan: initial scan error: %v", err)
		}
		log.Printf("scan: done, next run in %v", cfg.PollInterval)
	}()

	log.Println("reactor: initial run starting")
	go func() {
		if err := reactor.ProcessQueue(ctx); err != nil {
			log.Printf("reactor: initial queue error: %v", err)
		}
		log.Printf("reactor: done, next run in %v", reactorInterval)
	}()

	if cfg.WebEnabled {
		log.Printf("web: listening on %s:%d", cfg.WebHost, cfg.WebPort)
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
			log.Println("scan: run starting")
			go func() {
				if err := sc.Scan(ctx); err != nil {
					log.Printf("scan: error: %v", err)
				}
				log.Printf("scan: done, next run in %v", cfg.PollInterval)
			}()
		case <-reactorTicker.C:
			log.Println("reactor: run starting")
			go func() {
				if err := reactor.ProcessQueue(ctx); err != nil {
					log.Printf("reactor: queue error: %v", err)
				}
				log.Printf("reactor: done, next run in %v", reactorInterval)
			}()
		}
	}
}
