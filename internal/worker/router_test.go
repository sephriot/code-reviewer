package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

func TestRouterDispatchesByJobKind(t *testing.T) {
	t.Parallel()
	var received sqlite.Job
	router, err := NewRouter(map[string]Handler{
		"github.hydrate.v1": HandlerFunc(func(_ context.Context, job sqlite.Job) error {
			received = job
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	job := sqlite.Job{ID: "job-1", Kind: "github.hydrate.v1"}
	if err := router.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if received.ID != job.ID || received.Kind != job.Kind {
		t.Fatalf("received job = %+v, want %+v", received, job)
	}
}

func TestRouterMarksUnknownJobKindPermanent(t *testing.T) {
	t.Parallel()
	router, err := NewRouter(map[string]Handler{
		"known": HandlerFunc(func(context.Context, sqlite.Job) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = router.Handle(context.Background(), sqlite.Job{Kind: "unknown"})
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "unknown job kind") {
		t.Fatalf("Handle() error = %v, want permanent unknown-kind error", err)
	}
}

func TestNewRouterRejectsNilHandlers(t *testing.T) {
	t.Parallel()
	var typedNil *nilHandler
	for name, handlers := range map[string]map[string]Handler{
		"nil interface": {"known": nil},
		"typed nil":     {"known": typedNil},
		"nil func":      {"known": HandlerFunc(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRouter(handlers)
			if err == nil || !strings.Contains(err.Error(), "handler") {
				t.Fatalf("NewRouter() error = %v", err)
			}
		})
	}
}

func TestRouterPropagatesHandlerError(t *testing.T) {
	t.Parallel()
	want := errors.New("provider unavailable")
	router, err := NewRouter(map[string]Handler{
		"known": HandlerFunc(func(context.Context, sqlite.Job) error { return want }),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Handle(context.Background(), sqlite.Job{Kind: "known"}); !errors.Is(err, want) {
		t.Fatalf("Handle() error = %v, want %v", err, want)
	}
}

type nilHandler struct{}

func (*nilHandler) Handle(context.Context, sqlite.Job) error { return nil }

var _ Handler = (*Router)(nil)
