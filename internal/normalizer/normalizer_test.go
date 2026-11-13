package normalizer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shingoimota/slack-fetcher/internal/slackapi"
	"github.com/shingoimota/slack-fetcher/internal/urlparse"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestNormalizeThread_ExpandsMrkdwnBotAttachmentsAndFiles(t *testing.T) {
	client := slackapi.NewClient("xoxp-test",
		slackapi.WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/users.info"):
				uid := r.URL.Query().Get("user")
				h := http.Header{"Content-Type": []string{"application/json"}}
				switch uid {
				case "U01111111":
					return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U01111111","name":"alice","profile":{"display_name":"alice"}}}`))}, nil
				case "U02222222":
					return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U02222222","name":"bob","profile":{"display_name":"bob"}}}`))}, nil
				default:
					return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"user_not_found"}`))}, nil
				}
			case r.URL.String() == "https://files.slack.com/files-pri/T1-F1/error.log":
				h := http.Header{"Content-Type": []string{"text/plain"}}
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("2023-11-15T07:10:01Z ERROR db timeout"))}, nil
			case r.URL.String() == "https://files.slack.com/files-pri/T1-F2/spec.pdf":
				h := http.Header{"Content-Type": []string{"application/pdf"}}
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader("%PDF-1.4\nbinary"))}, nil
			default:
				t.Fatalf("unexpected request: %s", r.URL.String())
				return nil, nil
			}
		})),
	)

	attDir := filepath.Join(t.TempDir(), "attachments")
	ref := urlparse.ThreadRef{
		SourceURL:      "https://example.slack.com/archives/C01234567/p1700000099654321?thread_ts=1700000000.123456",
		WorkspaceHost:  "example.slack.com",
		ChannelID:      "C01234567",
		ThreadTS:       "1700000000.123456",
		FocusMessageTS: "1700000099.654321",
	}

	raw := []slackapi.RawMessage{
		{
			TS:   "1700000099.654321",
			User: "U02222222",
			Text: "See <!subteam^S123|@oncall> in <#C01234567|ops> &amp; <https://status.example.com|Status>",
		},
		{
			TS:   "1700000000.123456",
			User: "U01111111",
			Text: "Check this <@U02222222> and <@U99999999>",
			Files: []slackapi.RawFile{
				{
					ID:                 "F1",
					Name:               "error.log",
					Mimetype:           "text/plain",
					Size:               37,
					Mode:               "hosted",
					URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/error.log",
				},
				{
					ID:                 "F2",
					Name:               "../../evil/spec.pdf",
					Mimetype:           "application/pdf",
					Size:               15,
					Mode:               "hosted",
					URLPrivateDownload: "https://files.slack.com/files-pri/T1-F2/spec.pdf",
				},
				{
					ID:   "F3",
					Name: "deleted.txt",
					Mode: "tombstone",
				},
				{
					ID:        "F4",
					Name:      "GDoc",
					Mode:      "external",
					Permalink: "https://docs.google.com/document/d/123",
				},
			},
		},
		{
			TS:       "1700000105.000000",
			Username: "AlertBot",
			Attachments: []slackapi.RawAttachment{
				{
					Title:     "High Latency",
					TitleLink: "https://metrics.example.com/1",
					Text:      "p99 &gt; 2s",
					Fields: []slackapi.RawAttachmentField{
						{Title: "Service", Value: "api"},
					},
				},
			},
		},
	}

	jst := time.FixedZone("JST", 9*60*60)
	doc, err := NormalizeThread(context.Background(), ref, raw, NormalizeOptions{
		AttachmentDir:    attDir,
		MaxInlineBytes:   1024,
		MaxDownloadBytes: 4096,
		FetchedAt:        time.Date(2026, 11, 13, 12, 0, 0, 0, jst),
		Location:         jst,
		Client:           client,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(doc.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(doc.Messages))
	}
	// Sorted chronologically despite unsorted input
	if doc.Messages[0].TS != "1700000000.123456" || !doc.Messages[1].IsFocused {
		t.Fatalf("ordering/focus mismatch: %+v", doc.Messages)
	}
	if doc.Messages[0].Body != "Check this @bob and @U99999999" {
		t.Fatalf("msg[0].Body = %q", doc.Messages[0].Body)
	}
	if doc.Messages[1].Body != "See @oncall in #ops & [Status](https://status.example.com)" {
		t.Fatalf("msg[1].Body = %q", doc.Messages[1].Body)
	}
	if !strings.Contains(doc.Messages[2].Body, "**[High Latency](https://metrics.example.com/1)**") ||
		!strings.Contains(doc.Messages[2].Body, "p99 > 2s") ||
		!strings.Contains(doc.Messages[2].Body, "- **Service**: api") {
		t.Fatalf("msg[2].Body = %q", doc.Messages[2].Body)
	}

	atts := doc.Messages[0].Attachments
	if len(atts) != 4 {
		t.Fatalf("len(atts) = %d, want 4", len(atts))
	}
	if atts[0].Kind != AttachmentInline || atts[0].Content != "2023-11-15T07:10:01Z ERROR db timeout" {
		t.Fatalf("atts[0] = %+v", atts[0])
	}
	wantSavedPath := filepath.Join(attDir, "02_F2_spec.pdf")
	if atts[1].Kind != AttachmentSaved || atts[1].Path != wantSavedPath {
		t.Fatalf("atts[1] = %+v, want path %q", atts[1], wantSavedPath)
	}
	if _, err := os.Stat(wantSavedPath); err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
	if atts[2].Kind != AttachmentUnavailable {
		t.Fatalf("atts[2] = %+v, want unavailable", atts[2])
	}
	if atts[3].Kind != AttachmentExternal || atts[3].Permalink != "https://docs.google.com/document/d/123" {
		t.Fatalf("atts[3] = %+v", atts[3])
	}
	if doc.AttachmentDir != attDir {
		t.Fatalf("AttachmentDir = %q, want %q", doc.AttachmentDir, attDir)
	}
	if len(doc.Warnings) != 1 {
		t.Fatalf("expected 1 warning for U99999999, got %v", doc.Warnings)
	}
}

func TestNormalizeThread_BlocksFallbackAndSkipsOversizedFile(t *testing.T) {
	ref := urlparse.ThreadRef{
		SourceURL: "https://example.slack.com/archives/C01234567/p1700000000123456",
		ChannelID: "C01234567",
		ThreadTS:  "1700000000.123456",
	}
	raw := []slackapi.RawMessage{
		{
			TS:       "1700000000.123456",
			Username: "Workflow",
			Blocks: []json.RawMessage{
				json.RawMessage(`{"type":"header","text":{"type":"plain_text","text":"Deploy Notice"}}`),
				json.RawMessage(`{"type":"section","text":{"type":"mrkdwn","text":"Release &lt;v1.2&gt;"}}`),
			},
			Files: []slackapi.RawFile{
				{
					ID:                 "F9",
					Name:               "huge.zip",
					Mimetype:           "application/zip",
					Size:               100 << 20,
					Mode:               "hosted",
					URLPrivateDownload: "https://files.slack.com/files-pri/T1-F9/huge.zip",
				},
			},
		},
	}

	doc, err := NormalizeThread(context.Background(), ref, raw, NormalizeOptions{
		AttachmentDir:    t.TempDir(),
		MaxDownloadBytes: 50 << 20,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.AttachmentDir != "" {
		t.Fatalf("AttachmentDir should be empty when no file was saved, got %q", doc.AttachmentDir)
	}
	if doc.Messages[0].Body != "Deploy Notice\nRelease <v1.2>" {
		t.Fatalf("Body = %q", doc.Messages[0].Body)
	}
	if doc.Messages[0].Attachments[0].Kind != AttachmentSkipped {
		t.Fatalf("attachment = %+v, want skipped", doc.Messages[0].Attachments[0])
	}
}

func TestSanitizeFileName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"../../etc/passwd", "passwd"},
		{`..\..\evil\payload.txt`, "payload.txt"},
		{".hidden", "_hidden"},
		{"..", "attachment"},
		{"a\x00b\nc.log", "a_b_c.log"},
	}
	for _, tc := range tests {
		if got := SanitizeFileName(tc.in); got != tc.want {
			t.Errorf("SanitizeFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	t.Run("truncates multibyte UTF-8 on rune boundaries", func(t *testing.T) {
		withExt := SanitizeFileName(strings.Repeat("あ", 40) + ".pdf")
		if !utf8.ValidString(withExt) {
			t.Fatalf("withExt is invalid UTF-8: %x", withExt)
		}
		if len(withExt) > 120 || !strings.HasSuffix(withExt, ".pdf") {
			t.Fatalf("withExt = %q (len=%d)", withExt, len(withExt))
		}

		noExt := SanitizeFileName("a" + strings.Repeat("あ", 40))
		if !utf8.ValidString(noExt) {
			t.Fatalf("noExt is invalid UTF-8: %x", noExt)
		}
		if len(noExt) > 120 {
			t.Fatalf("noExt len = %d > 120", len(noExt))
		}
	})
}

func TestExpandMrkdwn_SpecialTokensAndEntities(t *testing.T) {
	users := map[string]slackapi.Author{
		"U1": {ID: "U1", DisplayName: "alice"},
	}
	in := "Ping <!here> <!channel> <!everyone> <@U1|old> <!date^1700000000^{date_short}|2023-11-15> <mailto:ops@example.com|ops@example.com> <https://example.com/q?a=1&amp;b=2>"
	got := expandMrkdwn(in, users)
	want := "Ping @here @channel @everyone @alice 2023-11-15 ops@example.com https://example.com/q?a=1&b=2"
	if got != want {
		t.Fatalf("expandMrkdwn() = %q, want %q", got, want)
	}
}

func TestNormalizeThread_RichTextPreformattedDynamicFenceAndFallbackSave(t *testing.T) {
	client := slackapi.NewClient("xoxp-test",
		slackapi.WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			h := http.Header{"Content-Type": []string{"text/plain"}}
			return &http.Response{
				StatusCode: 200,
				Header:     h,
				Body:       io.NopCloser(strings.NewReader(strings.Repeat("z", 40))),
			}, nil
		})),
	)

	attDir := filepath.Join(t.TempDir(), "att")
	ref := urlparse.ThreadRef{
		SourceURL: "https://example.slack.com/archives/C01234567/p1700000000123456",
		ChannelID: "C01234567",
		ThreadTS:  "1700000000.123456",
	}
	raw := []slackapi.RawMessage{
		{
			TS:       "1700000000.123456",
			Username: "Bot",
			Blocks: []json.RawMessage{
				json.RawMessage("{\"type\":\"rich_text\",\"elements\":[{\"type\":\"rich_text_preformatted\",\"elements\":[{\"type\":\"text\",\"text\":\"inside ``` code\"}]}]}"),
			},
			Files: []slackapi.RawFile{
				{
					ID:                 "F1",
					Name:               "big.txt",
					Mimetype:           "text/plain",
					Size:               0, // unknown in metadata; actual payload exceeds maxInlineBytes
					Mode:               "hosted",
					URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/big.txt",
				},
			},
		},
	}

	doc, err := NormalizeThread(context.Background(), ref, raw, NormalizeOptions{
		AttachmentDir:    attDir,
		MaxInlineBytes:   16,
		MaxDownloadBytes: 1024,
		Client:           client,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(doc.Messages[0].Body, "````\ninside ``` code\n````") {
		t.Fatalf("rich_text_preformatted did not use dynamic fence: %q", doc.Messages[0].Body)
	}
	if len(doc.Messages[0].Attachments) != 1 || doc.Messages[0].Attachments[0].Kind != AttachmentSaved {
		t.Fatalf("expected oversized text file with size=0 to fall back to Saved, got %+v", doc.Messages[0].Attachments)
	}
}

func TestNormalizeThread_CollectsUserIDsFromRichTextAndBotAttachmentFields(t *testing.T) {
	client := slackapi.NewClient("xoxp-test",
		slackapi.WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			h := http.Header{"Content-Type": []string{"application/json"}}
			switch r.URL.Query().Get("user") {
			case "U09999999":
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U09999999","name":"carol","profile":{"display_name":"carol"}}}`))}, nil
			case "U08888888":
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U08888888","name":"dave","profile":{"display_name":"dave"}}}`))}, nil
			case "U07777777":
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U07777777","name":"erin","profile":{"display_name":"erin"}}}`))}, nil
			default:
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error":"user_not_found"}`))}, nil
			}
		})),
	)

	ref := urlparse.ThreadRef{
		SourceURL: "https://example.slack.com/archives/C01234567/p1700000000123456",
		ChannelID: "C01234567",
		ThreadTS:  "1700000000.123456",
	}
	raw := []slackapi.RawMessage{
		{
			TS:       "1700000000.123456",
			Username: "Workflow",
			Blocks: []json.RawMessage{
				json.RawMessage(`{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"cc "},{"type":"user","user_id":"U09999999"}]}]}`),
			},
		},
		{
			TS:       "1700000001.000000",
			Username: "AlertBot",
			Attachments: []slackapi.RawAttachment{
				{AuthorName: "<@U08888888>"},
				{Fallback: "owner <@U07777777>"},
			},
		},
	}

	doc, err := NormalizeThread(context.Background(), ref, raw, NormalizeOptions{Client: client})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Messages[0].Body != "cc @carol" {
		t.Fatalf("msg[0].Body = %q, want %q", doc.Messages[0].Body, "cc @carol")
	}
	if !strings.Contains(doc.Messages[1].Body, "@dave") || !strings.Contains(doc.Messages[1].Body, "@erin") {
		t.Fatalf("msg[1].Body = %q, expected @dave and @erin", doc.Messages[1].Body)
	}
}

func TestNormalizeThread_ReturnsErrorOnContextCancellationDuringFileDownload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := slackapi.NewClient("xoxp-test",
		slackapi.WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cancel()
			return nil, context.Canceled
		})),
	)

	ref := urlparse.ThreadRef{
		SourceURL: "https://example.slack.com/archives/C01234567/p1700000000123456",
		ChannelID: "C01234567",
		ThreadTS:  "1700000000.123456",
	}
	raw := []slackapi.RawMessage{
		{
			TS:       "1700000000.123456",
			Username: "Bot",
			Files: []slackapi.RawFile{
				{
					ID:                 "F1",
					Name:               "spec.pdf",
					Mimetype:           "application/pdf",
					Size:               10,
					Mode:               "hosted",
					URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/spec.pdf",
				},
			},
		},
	}

	_, err := NormalizeThread(ctx, ref, raw, NormalizeOptions{
		AttachmentDir: t.TempDir(),
		Client:        client,
	})
	if err == nil {
		t.Fatalf("expected context cancellation error, got nil")
	}
}
