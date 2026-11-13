package slackapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body string, headers ...string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		h.Set(headers[i], headers[i+1])
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestFetchReplies_PaginatesAndRetries429(t *testing.T) {
	var calls atomic.Int32
	var slept []time.Duration

	client := NewClient("xoxp-test",
		WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("Authorization"); got != "Bearer xoxp-test" {
				t.Fatalf("Authorization = %q, want Bearer xoxp-test", got)
			}
			n := calls.Add(1)
			switch n {
			case 1:
				return jsonResponse(http.StatusTooManyRequests, `{"ok":false,"error":"ratelimited"}`, "Retry-After", "2"), nil
			case 2:
				if r.URL.Query().Get("cursor") != "" {
					t.Fatalf("unexpected cursor on first page: %q", r.URL.Query().Get("cursor"))
				}
				return jsonResponse(http.StatusOK, `{
					"ok": true,
					"messages": [{"ts": "1700000000.123456", "user": "U1", "text": "parent"}],
					"response_metadata": {"next_cursor": "cur-2"}
				}`), nil
			case 3:
				if r.URL.Query().Get("cursor") != "cur-2" {
					t.Fatalf("cursor = %q, want cur-2", r.URL.Query().Get("cursor"))
				}
				return jsonResponse(http.StatusOK, `{
					"ok": true,
					"messages": [{"ts": "1700000005.000001", "user": "U2", "text": "reply"}],
					"response_metadata": {"next_cursor": ""}
				}`), nil
			default:
				t.Fatalf("unexpected call #%d", n)
				return nil, nil
			}
		})),
		withSleep(func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}),
	)

	msgs, err := client.FetchReplies(context.Background(), "C01234567", "1700000000.123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Text != "parent" || msgs[1].Text != "reply" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("slept = %v, want [2s]", slept)
	}
}

func TestFetchReplies_StopsAfterMax429Retries(t *testing.T) {
	var calls atomic.Int32
	client := NewClient("xoxp-test",
		WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			return jsonResponse(http.StatusTooManyRequests, `{}`, "Retry-After", "0"), nil
		})),
		withSleep(func(context.Context, time.Duration) error { return nil }),
	)

	_, err := client.FetchReplies(context.Background(), "C01234567", "1700000000.123456")
	if err == nil {
		t.Fatalf("expected error after exhausting retries")
	}
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want 4 (1 initial + 3 retries)", calls.Load())
	}
}

func TestResolveUsers_CachesAndContinuesOnError(t *testing.T) {
	var calls atomic.Int32
	client := NewClient("xoxp-test",
		WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			uid := r.URL.Query().Get("user")
			if uid == "U1" {
				return jsonResponse(http.StatusOK, `{
					"ok": true,
					"user": {"id": "U1", "name": "alice.old", "profile": {"display_name": "alice"}}
				}`), nil
			}
			return jsonResponse(http.StatusOK, `{"ok": false, "error": "user_not_found"}`), nil
		})),
	)

	users, warnings, err := client.ResolveUsers(context.Background(), []string{"U1", "U1", "U_GONE"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want 1 warning", warnings)
	}
	if users["U1"].PreferredName() != "alice" {
		t.Fatalf("U1 PreferredName = %q, want alice", users["U1"].PreferredName())
	}
	if _, ok := users["U_GONE"]; ok {
		t.Fatalf("U_GONE should not be in resolved map")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (U1 cached)", calls.Load())
	}
}

func TestResolveUsers_AbortsImmediatelyOnAuthErrorOrContextCancel(t *testing.T) {
	t.Run("stops on missing_scope without querying remaining users", func(t *testing.T) {
		var calls atomic.Int32
		client := NewClient("xoxp-test",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return jsonResponse(http.StatusOK, `{"ok":false,"error":"missing_scope","needed":"users:read"}`), nil
			})),
		)

		_, _, err := client.ResolveUsers(context.Background(), []string{"U1", "U2", "U3"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.IsAuthOrPermission() || apiErr.Needed != "users:read" {
			t.Fatalf("err = %v, want missing_scope APIError", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("calls = %d, want 1 (must stop immediately on auth error)", calls.Load())
		}
	})

	t.Run("stops on canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		client := NewClient("xoxp-test",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Fatalf("HTTP should not be called with canceled context")
				return nil, nil
			})),
		)

		_, _, err := client.ResolveUsers(ctx, []string{"U1"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

func TestDownloadPrivateFile_StripsAuthOnExternalRedirect(t *testing.T) {
	var seenSlackAuth, seenS3Auth string

	client := NewClient("xoxp-secret",
		WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Hostname() {
			case "files.slack.com":
				seenSlackAuth = r.Header.Get("Authorization")
				h := make(http.Header)
				h.Set("Location", "https://example-bucket.s3.amazonaws.com/signed-obj")
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case "example-bucket.s3.amazonaws.com":
				seenS3Auth = r.Header.Get("Authorization")
				h := make(http.Header)
				h.Set("Content-Type", "application/pdf")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("%PDF-1.4\nbody")),
				}, nil
			default:
				t.Fatalf("unexpected host %q", r.URL.Hostname())
				return nil, nil
			}
		})),
	)

	dest := filepath.Join(t.TempDir(), "spec.pdf")
	_, n, saved, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/download/spec.pdf", -1, 1024, dest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !saved {
		t.Fatalf("expected saved=true")
	}
	if seenSlackAuth != "Bearer xoxp-secret" {
		t.Fatalf("slack host Authorization = %q, want Bearer xoxp-secret", seenSlackAuth)
	}
	if seenS3Auth != "" {
		t.Fatalf("external redirect leaked Authorization header: %q", seenS3Auth)
	}
	if n != int64(len("%PDF-1.4\nbody")) {
		t.Fatalf("written = %d, want %d", n, len("%PDF-1.4\nbody"))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read saved file: %v", err)
	}
	if string(got) != "%PDF-1.4\nbody" {
		t.Fatalf("saved bytes = %q", string(got))
	}
}

func TestDownloadPrivateFile_KeepsAuthOnTrustedSlackRedirect(t *testing.T) {
	var seenOriginAuth string

	client := NewClient("xoxp-secret",
		WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Hostname() {
			case "files.slack.com":
				h := make(http.Header)
				h.Set("Location", "https://files-origin.slack-files.com/obj")
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			case "files-origin.slack-files.com":
				seenOriginAuth = r.Header.Get("Authorization")
				h := make(http.Header)
				h.Set("Content-Type", "text/plain")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("ok")),
				}, nil
			default:
				t.Fatalf("unexpected host %q", r.URL.Hostname())
				return nil, nil
			}
		})),
	)

	dest := filepath.Join(t.TempDir(), "a.txt")
	if _, _, _, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/a.txt", -1, 1024, dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seenOriginAuth != "Bearer xoxp-secret" {
		t.Fatalf("trusted Slack redirect Authorization = %q, want Bearer xoxp-secret", seenOriginAuth)
	}
}

func TestDownloadPrivateFile_RejectsHTMLAndSizeOverflow(t *testing.T) {
	t.Run("rejects text/html login redirect", func(t *testing.T) {
		client := NewClient("xoxp-secret",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Content-Type", "text/html; charset=utf-8")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("<html>login</html>")),
				}, nil
			})),
		)

		dest := filepath.Join(t.TempDir(), "file.bin")
		_, _, _, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/file.bin", -1, 1024, dest)
		if !errors.Is(err, ErrUnexpectedHTMLResponse) {
			t.Fatalf("err = %v, want ErrUnexpectedHTMLResponse", err)
		}
	})

	t.Run("enforces maxDownloadBytes and removes temp files", func(t *testing.T) {
		client := NewClient("xoxp-secret",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Content-Type", "application/octet-stream")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("A"), 20))),
				}, nil
			})),
		)

		dir := t.TempDir()
		dest := filepath.Join(dir, "big.bin")
		_, _, _, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/big.bin", -1, 10, dest)
		if !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("err = %v, want ErrFileTooLarge", err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatalf("expected temp files to be cleaned up, found %d", len(entries))
		}
	})

	t.Run("rejects untrusted host before sending token", func(t *testing.T) {
		called := false
		client := NewClient("xoxp-secret",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("should not be called")
			})),
		)
		dest := filepath.Join(t.TempDir(), "x")
		_, _, _, err := client.FetchPrivateFile(context.Background(), "https://evil.example.com/file", -1, 10, dest)
		if !errors.Is(err, ErrUntrustedDownloadURL) {
			t.Fatalf("err = %v, want ErrUntrustedDownloadURL", err)
		}
		if called {
			t.Fatalf("transport was called for untrusted host")
		}
	})
}

func TestFetchPrivateFile_InlineOrFallbackSave(t *testing.T) {
	t.Run("returns inline bytes when utf8 and within limit", func(t *testing.T) {
		client := NewClient("xoxp-secret",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Content-Type", "text/plain")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader("hello log")),
				}, nil
			})),
		)

		dir := t.TempDir()
		dest := filepath.Join(dir, "error.log")
		inline, n, saved, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/error.log", 100, 1000, dest)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if saved || string(inline) != "hello log" || n != 9 {
			t.Fatalf("got inline=%q n=%d saved=%v", string(inline), n, saved)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("inline file should not be written to disk")
		}
	})

	t.Run("falls back to disk when exceeding maxInlineBytes", func(t *testing.T) {
		payload := strings.Repeat("x", 50)
		client := NewClient("xoxp-secret",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Content-Type", "text/plain")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(strings.NewReader(payload)),
				}, nil
			})),
		)

		dest := filepath.Join(t.TempDir(), "large.log")
		inline, n, saved, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/large.log", 20, 1000, dest)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !saved || len(inline) != 0 || n != 50 {
			t.Fatalf("got inline=%d n=%d saved=%v", len(inline), n, saved)
		}
		got, err := os.ReadFile(dest)
		if err != nil || string(got) != payload {
			t.Fatalf("disk content mismatch: err=%v len=%d", err, len(got))
		}
	})

	t.Run("falls back to disk when payload is invalid UTF-8", func(t *testing.T) {
		badUTF8 := []byte{0xff, 0xfe, 0xfd}
		client := NewClient("xoxp-secret",
			WithTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				h := make(http.Header)
				h.Set("Content-Type", "text/plain")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     h,
					Body:       io.NopCloser(bytes.NewReader(badUTF8)),
				}, nil
			})),
		)

		dest := filepath.Join(t.TempDir(), "latin1.txt")
		_, n, saved, err := client.FetchPrivateFile(context.Background(), "https://files.slack.com/files-pri/T1-F1/latin1.txt", 100, 1000, dest)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !saved || n != 3 {
			t.Fatalf("expected non-UTF8 text candidate to fall back to disk save, got saved=%v n=%d", saved, n)
		}
	})
}
