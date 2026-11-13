package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const DefaultKeychainService = "slack-fetcher"

var ErrTokenNotFound = errors.New("slack token not found")

// AuthStatus describes the currently active credential source without exposing raw secrets.
type AuthStatus struct {
	Source       string
	MaskedToken  string
	TeamID       string
	TeamName     string
	UserID       string
	Scope        string
	ExpiresAt    time.Time
	Refreshable  bool
	StoreDetails string
}

// Resolver resolves a Slack API token from SLACK_TOKEN or the local credential store,
// refreshing expiring PKCE OAuth tokens automatically when a refresh token is available.
type Resolver struct {
	Getenv          func(string) string
	GOOS            string
	KeychainService string
	ReadKeychain    func(ctx context.Context, service string) (string, error)
	Store           Store
	HTTPClient      *http.Client
	OAuthBaseURL    string
	Now             func() time.Time
}

// DefaultResolver returns a Resolver wired to the local environment and OS credential store.
func DefaultResolver(getenv func(string) string) Resolver {
	return Resolver{
		Getenv:          getenv,
		GOOS:            runtime.GOOS,
		KeychainService: DefaultKeychainService,
		Store:           DefaultStore(getenv),
		Now:             time.Now,
	}
}

func resolveKeychainService(getenv func(string) string, configured string) string {
	service := strings.TrimSpace(configured)
	if service == "" {
		service = DefaultKeychainService
	}
	if getenv != nil {
		if override := strings.TrimSpace(getenv("SLACK_FETCHER_KEYCHAIN_SERVICE")); override != "" {
			service = override
		}
	}
	return service
}

func (r Resolver) effectiveStore() Store {
	if r.Store != nil {
		return r.Store
	}
	service := resolveKeychainService(r.Getenv, r.KeychainService)
	if r.GOOS == "darwin" && r.ReadKeychain != nil {
		return legacyReadKeychainStore{
			service: service,
			readFn:  r.ReadKeychain,
		}
	}
	if r.GOOS == "darwin" {
		account := "default"
		if r.Getenv != nil {
			if u := strings.TrimSpace(r.Getenv("USER")); u != "" {
				account = u
			}
		}
		return KeychainStore{Service: service, Account: account, Run: runSecurityCommand}
	}
	if r.GOOS != "" && r.GOOS != "darwin" {
		return FileStore{Path: defaultCredentialsFilePath(r.Getenv)}
	}
	return DefaultStore(r.Getenv)
}

func (r Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// ResolveToken loads SLACK_TOKEN from the environment, falling back to the local credential store.
func (r Resolver) ResolveToken(ctx context.Context) (string, error) {
	if r.Getenv != nil {
		if token := strings.TrimSpace(r.Getenv("SLACK_TOKEN")); token != "" {
			return token, nil
		}
	}

	store := r.effectiveStore()
	creds, err := store.Load(ctx)
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			return "", fmt.Errorf("%w: run 'slack-fetcher auth login' or set SLACK_TOKEN (store: %s)", ErrTokenNotFound, store.Description())
		}
		return "", err
	}

	if needsRefresh(creds, r.now()) {
		refreshed, refreshErr := RefreshAccessToken(ctx, r.HTTPClient, r.OAuthBaseURL, creds, r.now())
		if refreshErr != nil {
			return "", fmt.Errorf("refresh Slack token failed (run 'slack-fetcher auth login' to re-authenticate): %w", refreshErr)
		}
		if saveErr := store.Save(ctx, refreshed); saveErr != nil {
			return "", fmt.Errorf("save refreshed credentials: %w", saveErr)
		}
		return refreshed.AccessToken, nil
	}

	return creds.AccessToken, nil
}

// Status inspects the active token source without triggering network calls.
func (r Resolver) Status(ctx context.Context) (AuthStatus, error) {
	store := r.effectiveStore()
	if r.Getenv != nil {
		if token := strings.TrimSpace(r.Getenv("SLACK_TOKEN")); token != "" {
			return AuthStatus{
				Source:       "env:SLACK_TOKEN",
				MaskedToken:  MaskToken(token),
				StoreDetails: store.Description(),
			}, nil
		}
	}

	creds, err := store.Load(ctx)
	if err != nil {
		return AuthStatus{StoreDetails: store.Description()}, err
	}
	return AuthStatus{
		Source:       store.Description(),
		MaskedToken:  MaskToken(creds.AccessToken),
		TeamID:       creds.TeamID,
		TeamName:     creds.TeamName,
		UserID:       creds.UserID,
		Scope:        creds.Scope,
		ExpiresAt:    creds.ExpiresAt,
		Refreshable:  creds.RefreshToken != "" && creds.ClientID != "",
		StoreDetails: store.Description(),
	}, nil
}

// MaskToken hides secret token characters while preserving the token type prefix.
func MaskToken(token string) string {
	t := strings.TrimSpace(token)
	if t == "" {
		return ""
	}
	if idx := strings.IndexByte(t, '-'); idx > 0 && idx < len(t)-1 {
		return t[:idx+1] + "****"
	}
	return "****"
}

func needsRefresh(creds Credentials, now time.Time) bool {
	if creds.RefreshToken == "" || creds.ClientID == "" || creds.ExpiresAt.IsZero() {
		return false
	}
	return !now.Add(5 * time.Minute).Before(creds.ExpiresAt)
}

type legacyReadKeychainStore struct {
	service string
	readFn  func(ctx context.Context, service string) (string, error)
}

func (s legacyReadKeychainStore) Description() string {
	return fmt.Sprintf("macOS Keychain (service=%q)", s.service)
}

func (s legacyReadKeychainStore) Load(ctx context.Context) (Credentials, error) {
	raw, err := s.readFn(ctx, s.service)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Credentials{}, fmt.Errorf("read macOS Keychain (%s): %w", s.service, err)
		}
		return Credentials{}, ErrTokenNotFound
	}
	return ParseStoredCredentials(raw)
}

func (s legacyReadKeychainStore) Save(context.Context, Credentials) error {
	return errors.New("read-only legacy keychain callback")
}

func (s legacyReadKeychainStore) Delete(context.Context) error {
	return errors.New("read-only legacy keychain callback")
}
