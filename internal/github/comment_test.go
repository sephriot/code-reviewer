package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	gogh "github.com/google/go-github/v68/github"
)

func TestCreateReviewCommentSendsCommitID(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	c := &Client{Client: gogh.NewClient(srv.Client())}
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = base

	err = c.CreateReviewComment(context.Background(), "spacelift-io", "backend", 15706, ReviewComment{
		File:     "foo.go",
		Line:     42,
		Message:  "needs a test",
		CommitID: "deadbeefcafebabe",
	})
	if err != nil {
		t.Fatalf("CreateReviewComment: %v", err)
	}

	if gotBody["commit_id"] != "deadbeefcafebabe" {
		t.Fatalf("commit_id = %#v, want deadbeefcafebabe", gotBody["commit_id"])
	}
	if gotBody["path"] != "foo.go" {
		t.Fatalf("path = %#v, want foo.go", gotBody["path"])
	}
	if gotBody["line"] != float64(42) {
		t.Fatalf("line = %#v, want 42", gotBody["line"])
	}
	if gotBody["side"] != "RIGHT" {
		t.Fatalf("side = %#v, want RIGHT", gotBody["side"])
	}
	if gotBody["body"] != "needs a test" {
		t.Fatalf("body = %#v", gotBody["body"])
	}
}

func TestCreateReviewCommentRequiresCommitID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := &Client{Client: gogh.NewClient(srv.Client())}
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = base

	err = c.CreateReviewComment(context.Background(), "o", "r", 1, ReviewComment{
		File: "a.go", Line: 1, Message: "x",
	})
	if err == nil {
		t.Fatal("expected error for missing commit_id")
	}
	if called {
		t.Fatal("API should not be called without commit_id")
	}
}
