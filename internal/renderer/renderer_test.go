package renderer

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/shingoimota/slack-fetcher/internal/normalizer"
)

func TestRenderMarkdown_UsesDynamicFenceAndFormatsAllSections(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	doc := normalizer.ThreadDocument{
		SourceURL:      "https://example.slack.com/archives/C01234567/p1700000099654321?thread_ts=1700000000.123456",
		ChannelID:      "C01234567",
		ThreadTS:       "1700000000.123456",
		FocusMessageTS: "1700000099.654321",
		FetchedAt:      time.Date(2026, 11, 13, 12, 0, 0, 0, jst),
		Participants:   []string{"@alice (U01111111)"},
		AttachmentDir:  "/tmp/slack-fetcher/C01234567_1700000000.123456",
		Messages: []normalizer.Message{
			{
				Index:       1,
				Timestamp:   time.Date(2023, 11, 15, 7, 13, 20, 0, jst),
				TS:          "1700000000.123456",
				AuthorLabel: "@alice",
				Body:        "Logs attached.",
				Attachments: []normalizer.Attachment{
					{
						Kind:     normalizer.AttachmentInline,
						Name:     "notes.md",
						MIMEType: "text/markdown",
						Size:     28,
						Language: "markdown",
						Content:  "```go\nfmt.Println(1)\n```",
					},
					{
						Kind:     normalizer.AttachmentSaved,
						Name:     "spec.pdf",
						MIMEType: "application/pdf",
						Size:     842110,
						Path:     "/tmp/slack-fetcher/C01234567_1700000000.123456/01_F1_spec.pdf",
					},
					{
						Kind:   normalizer.AttachmentUnavailable,
						Reason: "deleted file",
					},
					{
						Kind:      normalizer.AttachmentExternal,
						Name:      "Roadmap",
						Permalink: "https://docs.example.com/1",
					},
					{
						Kind:   normalizer.AttachmentSkipped,
						Name:   "huge.bin",
						Reason: "exceeds 50 MiB limit",
					},
				},
			},
			{
				Index:       2,
				Timestamp:   time.Date(2023, 11, 15, 7, 14, 59, 0, jst),
				TS:          "1700000099.654321",
				IsFocused:   true,
				AuthorLabel: "@bob",
				Body:        "Ack.",
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, doc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()

	checks := []string{
		`focus_message_ts: "1700000099.654321"`,
		`message_count: 2`,
		`attachment_dir: "/tmp/slack-fetcher/C01234567_1700000000.123456"`,
		`## [1] 2023-11-15 07:13:20+09:00 | @alice (ts: 1700000000.123456)`,
		"````markdown\n```go\nfmt.Println(1)\n```\n````",
		"- **Saved File**: `spec.pdf` (`application/pdf`, 842,110 bytes)",
		"- **Unavailable**: (deleted file)",
		"- **External Link**: [Roadmap](https://docs.example.com/1)",
		"- **Skipped**: `huge.bin` (reason: exceeds 50 MiB limit)",
		`## [2] 2023-11-15 07:14:59+09:00 | @bob (ts: 1700000099.654321) [FOCUSED]`,
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q\n--- OUTPUT ---\n%s", want, out)
		}
	}
}

func TestRenderMarkdown_EscapesAttachmentNamesAndLinks(t *testing.T) {
	doc := normalizer.ThreadDocument{
		SourceURL: "https://example.slack.com/archives/C01234567/p1700000000123456",
		ChannelID: "C01234567",
		ThreadTS:  "1700000000.123456",
		FetchedAt: time.Unix(1700000000, 0).UTC(),
		Messages: []normalizer.Message{
			{
				Index:       1,
				TS:          "1700000000.123456",
				AuthorLabel: "@alice",
				Attachments: []normalizer.Attachment{
					{
						Kind:     normalizer.AttachmentInline,
						Name:     "Fix `auth`\npanic",
						MIMEType: "text/plain",
						Size:     2,
						Language: "text",
						Content:  "ok",
					},
					{
						Kind:      normalizer.AttachmentExternal,
						Name:      "[RFC]\nAuth `v2`",
						Permalink: "https://docs.example.com/doc(1)\n?x=1",
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := RenderMarkdown(&buf, doc); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "- **Inline**: ``Fix `auth` panic`` (`text/plain`, 2 bytes)") {
		t.Fatalf("inline code span not safely escaped:\n%s", out)
	}
	if !strings.Contains(out, `- **External Link**: [\[RFC\] Auth `+"`v2`"+`](https://docs.example.com/doc%281%29?x=1)`) {
		t.Fatalf("external link not safely escaped:\n%s", out)
	}
}
