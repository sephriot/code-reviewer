package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/sephriot/code-reviewer/internal/migrate"
)

func main() {
	from := flag.String("from", "data/reviews.db", "path to legacy reviews.db")
	to := flag.String("to", "data/go-reviewer.db", "path to target go-reviewer.db")
	dryRun := flag.Bool("dry-run", false, "load and count candidates without writing")
	flag.Parse()

	if _, err := os.Stat(*from); err != nil {
		log.Fatalf("legacy db: %v", err)
	}
	if !*dryRun {
		if _, err := os.Stat(*to); err != nil {
			log.Fatalf("target db: %v", err)
		}
	}

	stats, err := migrate.Run(*from, *to, *dryRun)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	fmt.Printf("candidates=%d skipped_pending=%d prs_created=%d reviews_inserted=%d comments_inserted=%d skipped_existing=%d dry_run=%v\n",
		stats.Candidates, stats.SkippedPending, stats.PRsCreated, stats.ReviewsInserted, stats.CommentsInserted, stats.SkippedExisting, *dryRun)
}
