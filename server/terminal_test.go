package main

import "testing"

func TestOriginAllowed(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		siteURL string
		reqHost string
		want    bool
	}{
		{
			name:    "matches site URL host",
			origin:  "https://mm.example.com",
			siteURL: "https://mm.example.com",
			want:    true,
		},
		{
			name:    "site URL with path and port still matches on host",
			origin:  "https://mm.example.com:8065",
			siteURL: "https://mm.example.com:8065/subpath",
			want:    true,
		},
		{
			name:    "mismatched host is rejected",
			origin:  "https://evil.example.com",
			siteURL: "https://mm.example.com",
			want:    false,
		},
		{
			name:    "empty origin is rejected",
			origin:  "",
			siteURL: "https://mm.example.com",
			want:    false,
		},
		{
			name:    "unparseable origin is rejected",
			origin:  "://not a url",
			siteURL: "https://mm.example.com",
			want:    false,
		},
		{
			name:    "falls back to request host when no site URL",
			origin:  "https://localhost:8065",
			siteURL: "",
			reqHost: "localhost:8065",
			want:    true,
		},
		{
			name:    "fallback rejects mismatched request host",
			origin:  "https://localhost:8065",
			siteURL: "",
			reqHost: "other:8065",
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := originAllowed(tc.origin, tc.siteURL, tc.reqHost); got != tc.want {
				t.Errorf("originAllowed(%q, %q, %q) = %v, want %v", tc.origin, tc.siteURL, tc.reqHost, got, tc.want)
			}
		})
	}
}
