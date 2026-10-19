package main

import "testing"

func TestBrowserURL(t *testing.T) {
	for _, tc := range []struct {
		host string
		want string
	}{
		{"localhost", "http://localhost:8888"},
		{"127.0.0.1", "http://127.0.0.1:8888"},
		{"", "http://127.0.0.1:8888"},
		{"0.0.0.0", "http://127.0.0.1:8888"},
		{"::", "http://[::1]:8888"},
		{"::1", "http://[::1]:8888"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := browserURL(tc.host, 8888); got != tc.want {
				t.Fatalf("browserURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
