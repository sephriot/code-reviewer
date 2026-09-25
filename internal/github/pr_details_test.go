package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetPRDetailsIncludesLineChanges(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/repo/pulls/1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":1,"title":"change","user":{"login":"alice"},"head":{"sha":"abc"},"state":"open","additions":24,"deletions":7}`))
	}))
	defer srv.Close()

	client := testClient(t, srv, "alice")
	pr, err := client.GetPRDetails(context.Background(), "acme", "repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Additions == nil || *pr.Additions != 24 || pr.Deletions == nil || *pr.Deletions != 7 {
		t.Fatalf("line changes = %#v", pr)
	}
}
