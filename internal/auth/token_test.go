package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolver_EnvTakesPrecedenceOverKeychain(t *testing.T) {
	keychainCalled := false
	r := Resolver{
		Getenv: func(k string) string {
			if k == "SLACK_TOKEN" {
				return "  xoxp-env-token  "
			}
			return ""
		},
		GOOS: "darwin",
		ReadKeychain: func(ctx context.Context, service string) (string, error) {
			keychainCalled = true
			return "xoxp-keychain-token", nil
		},
	}

	got, err := r.ResolveToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "xoxp-env-token" {
		t.Fatalf("got %q, want %q", got, "xoxp-env-token")
	}
	if keychainCalled {
		t.Fatalf("keychain should not be queried when SLACK_TOKEN is set")
	}
}

func TestResolver_FallsBackToKeychainOnDarwin(t *testing.T) {
	var gotService string
	r := Resolver{
		Getenv: func(string) string { return "" },
		GOOS:   "darwin",
		ReadKeychain: func(ctx context.Context, service string) (string, error) {
			gotService = service
			return "xoxp-keychain-token\n", nil
		},
	}

	got, err := r.ResolveToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotService != DefaultKeychainService {
		t.Fatalf("service = %q, want %q", gotService, DefaultKeychainService)
	}
	if got != "xoxp-keychain-token" {
		t.Fatalf("got %q, want %q", got, "xoxp-keychain-token")
	}
}

func TestResolver_ReturnsErrTokenNotFoundWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	r := Resolver{
		Getenv: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return tmp
			}
			return ""
		},
		GOOS: "linux",
	}

	_, err := r.ResolveToken(context.Background())
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("err = %v, want ErrTokenNotFound", err)
	}
}

func TestResolver_ReportsOverriddenKeychainServiceAndPropagatesContextCancel(t *testing.T) {
	t.Run("error message includes overridden keychain service", func(t *testing.T) {
		r := Resolver{
			Getenv: func(k string) string {
				if k == "SLACK_FETCHER_KEYCHAIN_SERVICE" {
					return "corp-slack"
				}
				return ""
			},
			GOOS: "darwin",
			ReadKeychain: func(context.Context, string) (string, error) {
				return "", errors.New("exit status 44")
			},
		}

		_, err := r.ResolveToken(context.Background())
		if !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("err = %v, want ErrTokenNotFound", err)
		}
		if !strings.Contains(err.Error(), `"corp-slack"`) {
			t.Fatalf("error %q should mention overridden service \"corp-slack\"", err.Error())
		}
	})

	t.Run("propagates context cancellation instead of returning ErrTokenNotFound", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		r := Resolver{
			Getenv: func(string) string { return "" },
			GOOS:   "darwin",
			ReadKeychain: func(c context.Context, _ string) (string, error) {
				return "", c.Err()
			},
		}

		_, err := r.ResolveToken(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("context cancellation must not be masked as ErrTokenNotFound")
		}
	})
}

func TestResolver_AutoRefreshesExpiringCredentialsAndMasksStatus(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	store := FileStore{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(context.Background(), Credentials{
		AccessToken:  "xoxe.xoxp-old",
		RefreshToken: "xoxe-ref-1",
		ExpiresAt:    now.Add(2 * time.Minute),
		ClientID:     "111.222",
		TeamID:       "T1",
		TeamName:     "Corp",
		UserID:       "U1",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r := Resolver{
		Getenv: func(string) string { return "" },
		Store:  store,
		Now:    func() time.Time { return now },
		HTTPClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{
						"ok": true,
						"authed_user": {
							"id": "U1",
							"access_token": "xoxe.xoxp-fresh",
							"refresh_token": "xoxe-ref-2",
							"expires_in": 43200
						}
					}`)),
				}, nil
			}),
		},
	}

	got, err := r.ResolveToken(context.Background())
	if err != nil {
		t.Fatalf("ResolveToken: %v", err)
	}
	if got != "xoxe.xoxp-fresh" {
		t.Fatalf("got %q, want xoxe.xoxp-fresh", got)
	}

	status, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.MaskedToken != "xoxe.xoxp-****" || status.TeamName != "Corp" || !status.Refreshable {
		t.Fatalf("unexpected status: %+v", status)
	}
	if strings.Contains(status.MaskedToken, "fresh") {
		t.Fatalf("status exposed raw secret: %q", status.MaskedToken)
	}
}
