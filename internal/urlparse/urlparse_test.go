package urlparse

import "testing"

func TestParseThreadURL_ValidURLs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want ThreadRef
	}{
		{
			name: "parent message permalink",
			raw:  "https://example.slack.com/archives/C01234567/p1700000000123456",
			want: ThreadRef{
				SourceURL:     "https://example.slack.com/archives/C01234567/p1700000000123456",
				WorkspaceHost: "example.slack.com",
				ChannelID:     "C01234567",
				ThreadTS:      "1700000000.123456",
			},
		},
		{
			name: "reply permalink prioritizes thread_ts query and keeps focus_message_ts",
			raw:  "https://example.slack.com/archives/C01234567/p1700000099654321?thread_ts=1700000000.123456&cid=C01234567",
			want: ThreadRef{
				SourceURL:      "https://example.slack.com/archives/C01234567/p1700000099654321?thread_ts=1700000000.123456&cid=C01234567",
				WorkspaceHost:  "example.slack.com",
				ChannelID:      "C01234567",
				ThreadTS:       "1700000000.123456",
				FocusMessageTS: "1700000099.654321",
			},
		},
		{
			name: "parent permalink with matching thread_ts query clears focus_message_ts",
			raw:  "https://example.slack.com/archives/G09876543/p1700000000123456?thread_ts=1700000000.123456&cid=G09876543",
			want: ThreadRef{
				SourceURL:     "https://example.slack.com/archives/G09876543/p1700000000123456?thread_ts=1700000000.123456&cid=G09876543",
				WorkspaceHost: "example.slack.com",
				ChannelID:     "G09876543",
				ThreadTS:      "1700000000.123456",
			},
		},
		{
			name: "app.slack.com client thread URL",
			raw:  "https://app.slack.com/client/T01111111/C01234567/thread/C01234567-1700000000.123456",
			want: ThreadRef{
				SourceURL:     "https://app.slack.com/client/T01111111/C01234567/thread/C01234567-1700000000.123456",
				WorkspaceHost: "app.slack.com",
				ChannelID:     "C01234567",
				ThreadTS:      "1700000000.123456",
			},
		},
		{
			name: "DM channel ID prefix D",
			raw:  "https://myorg.enterprise.slack.com/archives/D0123456789/p1700000000123456",
			want: ThreadRef{
				SourceURL:     "https://myorg.enterprise.slack.com/archives/D0123456789/p1700000000123456",
				WorkspaceHost: "myorg.enterprise.slack.com",
				ChannelID:     "D0123456789",
				ThreadTS:      "1700000000.123456",
			},
		},
		{
			name: "app.slack.com client thread URL with query and trailing slash",
			raw:  "https://app.slack.com/client/T01111111/C01234567/thread/C01234567-1700000000.123456/?foo=bar",
			want: ThreadRef{
				SourceURL:     "https://app.slack.com/client/T01111111/C01234567/thread/C01234567-1700000000.123456/?foo=bar",
				WorkspaceHost: "app.slack.com",
				ChannelID:     "C01234567",
				ThreadTS:      "1700000000.123456",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseThreadURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseThreadURL(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseThreadURL(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseThreadURL_InvalidURLs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty string", raw: "   "},
		{name: "http scheme rejected", raw: "http://example.slack.com/archives/C01234567/p1700000000123456"},
		{name: "non-slack host rejected", raw: "https://evil-slack.com/archives/C01234567/p1700000000123456"},
		{name: "channel only URL without message", raw: "https://example.slack.com/archives/C01234567"},
		{name: "invalid channel prefix", raw: "https://example.slack.com/archives/U01234567/p1700000000123456"},
		{name: "short p timestamp", raw: "https://example.slack.com/archives/C01234567/p1700000000"},
		{name: "non-digit p timestamp", raw: "https://example.slack.com/archives/C01234567/p170000000012345a"},
		{name: "extra archives path segment", raw: "https://example.slack.com/archives/C01234567/p1700000000123456/extra"},
		{name: "malformed thread_ts query", raw: "https://example.slack.com/archives/C01234567/p1700000099654321?thread_ts=not-a-ts"},
		{name: "malformed client thread segment", raw: "https://app.slack.com/client/T01111111/C01234567/thread/C01234567"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseThreadURL(tc.raw); err == nil {
				t.Fatalf("ParseThreadURL(%q) expected error, got %+v", tc.raw, got)
			}
		})
	}
}
