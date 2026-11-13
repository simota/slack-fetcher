package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shingoimota/slack-fetcher/internal/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestRun_EndToEndSuccessAndExitCodes(t *testing.T) {
	t.Run("writes structured markdown to output file and saves binary attachment", func(t *testing.T) {
		tmp := t.TempDir()
		attDir := filepath.Join(tmp, "att")
		outFile := filepath.Join(tmp, "out", "thread.md")

		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/conversations.replies"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{
						"ok": true,
						"messages": [{
							"ts": "1700000000.123456",
							"user": "U1",
							"text": "Hello &lt;team&gt;",
							"files": [{
								"id": "F1",
								"name": "app.log",
								"mimetype": "text/plain",
								"size": 9,
								"mode": "hosted",
								"url_private_download": "https://files.slack.com/files-pri/T1-F1/app.log"
							},{
								"id": "F2",
								"name": "diagram.png",
								"mimetype": "image/png",
								"size": 4,
								"mode": "hosted",
								"url_private_download": "https://files.slack.com/files-pri/T1-F2/diagram.png"
							}]
						}]
					}`)),
				}, nil
			case strings.HasSuffix(r.URL.Path, "/users.info"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U1","name":"alice","profile":{"display_name":"alice"}}}`)),
				}, nil
			case r.URL.String() == "https://files.slack.com/files-pri/T1-F1/app.log":
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/plain"}},
					Body:       io.NopCloser(strings.NewReader("line1=ok\n")),
				}, nil
			case r.URL.String() == "https://files.slack.com/files-pri/T1-F2/diagram.png":
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"image/png"}},
					Body:       io.NopCloser(strings.NewReader("PNG!")),
				}, nil
			default:
				t.Fatalf("unexpected request: %s", r.URL.String())
				return nil, nil
			}
		})

		var stdout, stderr bytes.Buffer
		code := run(
			[]string{
				"--attachment-dir", attDir,
				"-o", outFile,
				"https://example.slack.com/archives/C01234567/p1700000000123456",
			},
			deps{
				getenv:    func(k string) string { if k == "SLACK_TOKEN" { return "xoxp-test" }; return "" },
				transport: rt,
				now:       func() time.Time { return time.Unix(1700000100, 0) },
			},
			&stdout,
			&stderr,
		)

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout should be empty when -o is used, got %q", stdout.String())
		}
		md, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatalf("read output file: %v", err)
		}
		s := string(md)
		if !strings.Contains(s, "Hello <team>") ||
			!strings.Contains(s, "line1=ok") ||
			!strings.Contains(s, filepath.Join(attDir, "02_F2_diagram.png")) {
			t.Fatalf("unexpected markdown:\n%s", s)
		}
	})

	t.Run("writes markdown to stdout and warnings to stderr", func(t *testing.T) {
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/conversations.replies"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{
						"ok": true,
						"messages": [{"ts": "1700000000.123456", "user": "U1", "text": "hi"}]
					}`)),
				}, nil
			case strings.HasSuffix(r.URL.Path, "/users.info"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"user_not_found"}`)),
				}, nil
			default:
				t.Fatalf("unexpected request: %s", r.URL.String())
				return nil, nil
			}
		})

		var stdout, stderr bytes.Buffer
		code := run(
			[]string{"https://example.slack.com/archives/C01234567/p1700000000123456"},
			deps{
				tokenFn:   func(context.Context) (string, error) { return "xoxp-test", nil },
				transport: rt,
				now:       func() time.Time { return time.Unix(1700000100, 0) },
			},
			&stdout,
			&stderr,
		)

		if code != exitOK {
			t.Fatalf("exit code = %d, want %d", code, exitOK)
		}
		if !strings.Contains(stdout.String(), "@U1") {
			t.Fatalf("stdout missing fallback user ID: %s", stdout.String())
		}
		if !strings.Contains(stderr.String(), "warning:") {
			t.Fatalf("stderr should contain user lookup warning, got %q", stderr.String())
		}
	})

	t.Run("exit 2 on invalid URL or rejected --token flag", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		if code := run([]string{"https://not-slack.example.com/foo"}, deps{}, &stdout, &stderr); code != exitUsage {
			t.Fatalf("invalid URL exit = %d, want %d", code, exitUsage)
		}
		stderr.Reset()
		if code := run([]string{"--token", "xoxp-leak", "https://example.slack.com/archives/C01234567/p1700000000123456"}, deps{}, &stdout, &stderr); code != exitUsage {
			t.Fatalf("--token flag exit = %d, want %d", code, exitUsage)
		}
	})

	t.Run("exit 3 on missing token or permission error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(
			[]string{"https://example.slack.com/archives/C01234567/p1700000000123456"},
			deps{
				tokenFn: func(context.Context) (string, error) {
					return "", auth.ErrTokenNotFound
				},
			},
			&stdout,
			&stderr,
		)
		if code != exitAuth {
			t.Fatalf("missing token exit = %d, want %d", code, exitAuth)
		}

		stderr.Reset()
		code = run(
			[]string{"https://example.slack.com/archives/C01234567/p1700000000123456"},
			deps{
				tokenFn: func(context.Context) (string, error) { return "xoxp-test", nil },
				transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"missing_scope","needed":"channels:history","provided":"users:read"}`)),
					}, nil
				}),
			},
			&stdout,
			&stderr,
		)
		if code != exitAuth || !strings.Contains(stderr.String(), "channels:history") {
			t.Fatalf("missing_scope exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("exit 4 on thread_not_found", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(
			[]string{"https://example.slack.com/archives/C01234567/p1700000000123456"},
			deps{
				tokenFn: func(context.Context) (string, error) { return "xoxp-test", nil },
				transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"thread_not_found"}`)),
					}, nil
				}),
			},
			&stdout,
			&stderr,
		)
		if code != exitFetchFail {
			t.Fatalf("thread_not_found exit = %d, want %d", code, exitFetchFail)
		}
	})

	t.Run("exit 3 on not_in_channel", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run(
			[]string{"https://example.slack.com/archives/C01234567/p1700000000123456"},
			deps{
				tokenFn: func(context.Context) (string, error) { return "xoxb-bot", nil },
				transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"not_in_channel"}`)),
					}, nil
				}),
			},
			&stdout,
			&stderr,
		)
		if code != exitAuth || !strings.Contains(stderr.String(), "xoxp-") {
			t.Fatalf("not_in_channel exit=%d stderr=%q", code, stderr.String())
		}
	})

	t.Run("exit 4 and writes no output when timeout expires after FetchReplies", func(t *testing.T) {
		outFile := filepath.Join(t.TempDir(), "should-not-exist.md")
		var stdout, stderr bytes.Buffer

		code := run(
			[]string{
				"--timeout", "15ms",
				"-o", outFile,
				"https://example.slack.com/archives/C01234567/p1700000000123456",
			},
			deps{
				tokenFn: func(context.Context) (string, error) { return "xoxp-test", nil },
				transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/conversations.replies") {
						return &http.Response{
							StatusCode: 200,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(`{"ok":true,"messages":[{"ts":"1700000000.123456","user":"U1","text":"hi"}]}`)),
						}, nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				}),
			},
			&stdout,
			&stderr,
		)

		if code != exitFetchFail {
			t.Fatalf("timeout after FetchReplies exit = %d, want %d (stderr: %s)", code, exitFetchFail, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must be empty on timeout, got %q", stdout.String())
		}
		if _, err := os.Stat(outFile); !os.IsNotExist(err) {
			t.Fatalf("output file should not be created on timeout")
		}
	})

	t.Run("exit 3 immediately when users.info returns missing_scope", func(t *testing.T) {
		var userCalls atomic.Int32
		var stdout, stderr bytes.Buffer

		code := run(
			[]string{"https://example.slack.com/archives/C01234567/p1700000000123456"},
			deps{
				tokenFn: func(context.Context) (string, error) { return "xoxp-test", nil },
				transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/conversations.replies") {
						return &http.Response{
							StatusCode: 200,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body: io.NopCloser(strings.NewReader(`{
								"ok": true,
								"messages": [
									{"ts": "1700000000.123456", "user": "U1", "text": "one"},
									{"ts": "1700000001.000000", "user": "U2", "text": "two"}
								]
							}`)),
						}, nil
					}
					userCalls.Add(1)
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"missing_scope","needed":"users:read"}`)),
					}, nil
				}),
			},
			&stdout,
			&stderr,
		)

		if code != exitAuth {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitAuth, stderr.String())
		}
		if userCalls.Load() != 1 {
			t.Fatalf("users.info calls = %d, want 1", userCalls.Load())
		}
		if !strings.Contains(stderr.String(), "users:read") {
			t.Fatalf("stderr = %q, expected users:read", stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout must be empty on missing_scope, got %q", stdout.String())
		}
	})
}

func TestRunAuth_PKCELoginStatusLogoutAndTokenStdin(t *testing.T) {
	t.Run("PKCE OAuth login, status masking, thread fetch, and logout", func(t *testing.T) {
		store := auth.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}

		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/oauth.v2.access"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{
						"ok": true,
						"team": {"id": "T01", "name": "InternalOrg"},
						"authed_user": {
							"id": "U01",
							"scope": "channels:history,files:read,users:read",
							"access_token": "xoxe.xoxp-secret-token-value",
							"refresh_token": "xoxe-refresh-1",
							"expires_in": 43200
						}
					}`)),
				}, nil
			case strings.HasSuffix(r.URL.Path, "/conversations.replies"):
				if got := r.Header.Get("Authorization"); got != "Bearer xoxe.xoxp-secret-token-value" {
					t.Fatalf("Authorization = %q", got)
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"messages":[{"ts":"1700000000.123456","user":"U01","text":"hi"}]}`)),
				}, nil
			case strings.HasSuffix(r.URL.Path, "/users.info"):
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"user":{"id":"U01","name":"alice","profile":{"display_name":"alice"}}}`)),
				}, nil
			default:
				t.Fatalf("unexpected request: %s", r.URL.String())
				return nil, nil
			}
		})

		d := deps{
			getenv:    func(string) string { return "" },
			transport: rt,
			authStore: store,
			now:       func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) },
			openBrowser: func(authURL string) error {
				parsed, err := url.Parse(authURL)
				if err != nil {
					return err
				}
				q := parsed.Query()
				redirectURI := q.Get("redirect_uri")
				state := q.Get("state")
				go func() {
					resp, reqErr := http.Get(redirectURI + "?code=code-xyz&state=" + url.QueryEscape(state))
					if reqErr == nil {
						_ = resp.Body.Close()
					}
				}()
				return nil
			},
		}

		var stdout, stderr bytes.Buffer
		code := run([]string{"auth", "login", "--client-id", "111.222", "--port", "0"}, d, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("auth login exit = %d, stderr=%s", code, stderr.String())
		}
		if strings.Contains(stdout.String(), "secret-token-value") {
			t.Fatalf("auth login leaked raw token in stdout: %s", stdout.String())
		}

		stdout.Reset()
		stderr.Reset()
		code = run([]string{"auth", "status"}, d, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("auth status exit = %d, stderr=%s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "InternalOrg") || strings.Contains(stdout.String(), "secret-token-value") {
			t.Fatalf("unexpected auth status output: %s", stdout.String())
		}

		stdout.Reset()
		stderr.Reset()
		code = run([]string{"https://example.slack.com/archives/C01234567/p1700000000123456"}, d, &stdout, &stderr)
		if code != exitOK || !strings.Contains(stdout.String(), "@alice") {
			t.Fatalf("thread fetch after login failed: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}

		stdout.Reset()
		stderr.Reset()
		code = run([]string{"auth", "logout"}, d, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("auth logout exit = %d, stderr=%s", code, stderr.String())
		}

		stdout.Reset()
		stderr.Reset()
		code = run([]string{"auth", "status"}, d, &stdout, &stderr)
		if code != exitAuth {
			t.Fatalf("auth status after logout exit = %d, want %d", code, exitAuth)
		}
	})

	t.Run("auth login --token-stdin stores manual token", func(t *testing.T) {
		store := auth.FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
		var stdout, stderr bytes.Buffer
		d := deps{
			getenv:    func(string) string { return "" },
			stdin:     strings.NewReader("xoxp-manual-secret\n"),
			authStore: store,
		}

		code := run([]string{"auth", "login", "--token-stdin"}, d, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("auth login --token-stdin exit = %d, stderr=%s", code, stderr.String())
		}
		if strings.Contains(stdout.String(), "manual-secret") {
			t.Fatalf("leaked manual token in stdout: %s", stdout.String())
		}

		creds, err := store.Load(context.Background())
		if err != nil || creds.AccessToken != "xoxp-manual-secret" {
			t.Fatalf("loaded creds = %+v, err = %v", creds, err)
		}
	})
}
