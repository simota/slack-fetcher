package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestGeneratePKCEAndBuildAuthorizeURL(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	sum := sha256.Sum256([]byte(pkce.Verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.Challenge != wantChallenge {
		t.Fatalf("Challenge = %q, want %q", pkce.Challenge, wantChallenge)
	}

	rawURL := BuildAuthorizeURL("", "111.222", "channels:history,files:read", "http://localhost:53682/callback", pkce)
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "111.222" ||
		q.Get("user_scope") != "channels:history,files:read" ||
		q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != pkce.Challenge ||
		q.Get("state") != pkce.State ||
		q.Get("client_secret") != "" {
		t.Fatalf("unexpected authorize query: %v", q)
	}
}

func TestWaitForOAuthCallback(t *testing.T) {
	t.Run("accepts valid state and returns code", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		done := make(chan int, 1)
		code, err := WaitForOAuthCallback(ctx, 0, "expected-state", func(redirectURI string) {
			go func() {
				resp, reqErr := http.Get(redirectURI + "?code=auth-code-123&state=expected-state")
				if reqErr != nil {
					done <- 0
					return
				}
				_ = resp.Body.Close()
				done <- resp.StatusCode
			}()
		})
		if err != nil {
			t.Fatalf("WaitForOAuthCallback: %v", err)
		}
		if code != "auth-code-123" {
			t.Fatalf("code = %q, want auth-code-123", code)
		}
		if st := <-done; st != http.StatusOK {
			t.Fatalf("HTTP status = %d, want 200", st)
		}
	})

	t.Run("rejects mismatched state", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		statusCh := make(chan int, 1)
		_, err := WaitForOAuthCallback(ctx, 0, "expected-state", func(redirectURI string) {
			go func() {
				resp, reqErr := http.Get(redirectURI + "?code=auth-code-123&state=wrong-state")
				if reqErr != nil {
					statusCh <- 0
					return
				}
				_ = resp.Body.Close()
				statusCh <- resp.StatusCode
			}()
		})
		if err == nil || !strings.Contains(err.Error(), "state mismatch") {
			t.Fatalf("err = %v, want state mismatch", err)
		}
		if status := <-statusCh; status != http.StatusBadRequest {
			t.Fatalf("HTTP status = %d, want 400", status)
		}
	})
}

func TestExchangeCodeAndRefreshAccessToken(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			bodyBytes, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(bodyBytes))

			if form.Get("client_secret") != "" {
				t.Fatalf("PKCE exchange must not send client_secret")
			}

			switch form.Get("grant_type") {
			case "authorization_code":
				if form.Get("code") != "code-1" || form.Get("code_verifier") != "ver-1" {
					t.Fatalf("unexpected exchange form: %v", form)
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{
						"ok": true,
						"team": {"id": "T1", "name": "Acme Corp"},
						"authed_user": {
							"id": "U1",
							"scope": "channels:history,files:read",
							"access_token": "xoxe.xoxp-new",
							"refresh_token": "xoxe-ref-1",
							"expires_in": 43200
						}
					}`)),
				}, nil
			case "refresh_token":
				if form.Get("refresh_token") != "xoxe-ref-1" {
					t.Fatalf("unexpected refresh form: %v", form)
				}
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{
						"ok": true,
						"authed_user": {
							"id": "U1",
							"access_token": "xoxe.xoxp-refreshed",
							"refresh_token": "xoxe-ref-2",
							"expires_in": 43200
						}
					}`)),
				}, nil
			default:
				t.Fatalf("unexpected grant_type: %s", form.Get("grant_type"))
				return nil, nil
			}
		}),
	}

	creds, err := ExchangeCode(context.Background(), httpClient, "", "111.222", "code-1", "ver-1", "http://localhost:53682/callback", now)
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if creds.AccessToken != "xoxe.xoxp-new" || creds.TeamName != "Acme Corp" || creds.ExpiresAt.IsZero() {
		t.Fatalf("unexpected creds: %+v", creds)
	}

	refreshed, err := RefreshAccessToken(context.Background(), httpClient, "", creds, now.Add(12*time.Hour))
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if refreshed.AccessToken != "xoxe.xoxp-refreshed" || refreshed.RefreshToken != "xoxe-ref-2" || refreshed.TeamName != "Acme Corp" {
		t.Fatalf("unexpected refreshed creds: %+v", refreshed)
	}
}
