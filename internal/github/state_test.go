package github

import "testing"

func TestNormalizePRState(t *testing.T) {
	tests := []struct {
		name    string
		ghState string
		merged  bool
		want    string
	}{
		{name: "open", ghState: "open", merged: false, want: "open"},
		{name: "closed not merged", ghState: "closed", merged: false, want: "closed"},
		{name: "merged flag wins", ghState: "closed", merged: true, want: "merged"},
		{name: "merged even if open weird", ghState: "open", merged: true, want: "merged"},
		{name: "empty defaults open", ghState: "", merged: false, want: "open"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizePRState(tt.ghState, tt.merged)
			if got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
}
