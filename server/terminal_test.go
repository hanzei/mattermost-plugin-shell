package main

import "testing"

func TestResolveShell(t *testing.T) {
	detected := []shellInfo{
		{Name: "bash", Path: "/bin/bash"},
		{Name: "zsh", Path: "/bin/zsh"},
		{Name: "sh", Path: "/bin/sh"},
	}

	tests := []struct {
		name      string
		requested string
		saved     string
		detected  []shellInfo
		wantName  string
		wantOK    bool
	}{
		{
			name:      "valid request wins",
			requested: "zsh",
			saved:     "bash",
			detected:  detected,
			wantName:  "zsh",
			wantOK:    true,
		},
		{
			name:      "unknown request is ignored, falls back to saved",
			requested: "powershell",
			saved:     "zsh",
			detected:  detected,
			wantName:  "zsh",
			wantOK:    true,
		},
		{
			name:     "saved preference used when no request",
			saved:    "sh",
			detected: detected,
			wantName: "sh",
			wantOK:   true,
		},
		{
			name:     "saved preference no longer present falls back to default",
			saved:    "fish",
			detected: detected,
			wantName: "bash",
			wantOK:   true,
		},
		{
			name:     "default prefers bash then sh",
			detected: []shellInfo{{Name: "sh", Path: "/bin/sh"}, {Name: "zsh", Path: "/bin/zsh"}},
			wantName: "sh",
			wantOK:   true,
		},
		{
			name:     "default falls through to first detected",
			detected: []shellInfo{{Name: "zsh", Path: "/bin/zsh"}, {Name: "fish", Path: "/usr/bin/fish"}},
			wantName: "zsh",
			wantOK:   true,
		},
		{
			name:     "no shells detected",
			detected: nil,
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveShell(tc.requested, tc.saved, tc.detected)
			if ok != tc.wantOK {
				t.Fatalf("resolveShell ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Name != tc.wantName {
				t.Errorf("resolveShell name = %q, want %q", got.Name, tc.wantName)
			}
		})
	}
}

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
