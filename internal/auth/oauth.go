package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	DefaultOAuthAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	DefaultSlackAPIBase      = "https://slack.com/api"
	DefaultRedirectPort      = 53682
	DefaultUserScope         = "channels:history,files:read,users:read,groups:history,im:history,mpim:history"
)

// DefaultClientID can be injected at build time for pre-configured distribution via:
// -ldflags "-X github.com/shingoimota/slack-fetcher/internal/auth.DefaultClientID=<id>"
var DefaultClientID = ""

// PKCEPair holds the PKCE verifier, S256 challenge, and CSRF state token.
type PKCEPair struct {
	Verifier  string
	Challenge string
	State     string
}

// GeneratePKCE generates cryptographically random PKCE and state parameters.
func GeneratePKCE() (PKCEPair, error) {
	verifierBytes := make([]byte, 48)
	if _, err := rand.Read(verifierBytes); err != nil {
		return PKCEPair{}, fmt.Errorf("generate code_verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	stateBytes := make([]byte, 24)
	if _, err := rand.Read(stateBytes); err != nil {
		return PKCEPair{}, fmt.Errorf("generate oauth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	return PKCEPair{
		Verifier:  verifier,
		Challenge: challenge,
		State:     state,
	}, nil
}

// BuildAuthorizeURL builds the Slack OAuth v2 PKCE authorization URL.
func BuildAuthorizeURL(authorizeBase, clientID, userScope, redirectURI string, pkce PKCEPair) string {
	if strings.TrimSpace(authorizeBase) == "" {
		authorizeBase = DefaultOAuthAuthorizeURL
	}
	if strings.TrimSpace(userScope) == "" {
		userScope = DefaultUserScope
	}
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("user_scope", userScope)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", pkce.State)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	return strings.TrimRight(authorizeBase, "?") + "?" + q.Encode()
}

// WaitForOAuthCallback starts a loopback-only HTTP server on 127.0.0.1:<port>
// and waits for Slack's /callback redirect containing the authorization code.
func WaitForOAuthCallback(ctx context.Context, port int, expectedState string, onListening func(redirectURI string)) (string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer ln.Close()

	actualPort := ln.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", actualPort)

	type callbackResult struct {
		code string
		err  error
	}
	resCh := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if oauthErr := strings.TrimSpace(q.Get("error")); oauthErr != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "<html><body><h3>Slack authorization failed: %s</h3><p>You can close this tab.</p></body></html>", html.EscapeString(oauthErr))
			select {
			case resCh <- callbackResult{err: fmt.Errorf("slack oauth denied: %s", oauthErr)}:
			default:
			}
			return
		}

		gotState := q.Get("state")
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(expectedState)) != 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<html><body><h3>Invalid OAuth state</h3><p>Please retry login from CLI.</p></body></html>"))
			select {
			case resCh <- callbackResult{err: errors.New("oauth state mismatch")}:
			default:
			}
			return
		}

		code := strings.TrimSpace(q.Get("code"))
		if code == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<html><body><h3>Missing authorization code</h3></body></html>"))
			select {
			case resCh <- callbackResult{err: errors.New("missing authorization code in callback")}:
			default:
			}
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body><h3>Authentication complete</h3><p>You can close this window and return to the terminal.</p></body></html>"))
		select {
		case resCh <- callbackResult{code: code}:
		default:
		}
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		_ = srv.Serve(ln)
	}()

	if onListening != nil {
		onListening(redirectURI)
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-resCh:
		return res.code, res.err
	}
}

type oauthAccessResponse struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	AuthedUser struct {
		ID           string `json:"id"`
		Scope        string `json:"scope"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	} `json:"authed_user"`
	Team struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"team"`
}

// ExchangeCode exchanges an OAuth authorization code with PKCE code_verifier for user credentials.
func ExchangeCode(ctx context.Context, httpClient *http.Client, apiBase, clientID, code, verifier, redirectURI string, now time.Time) (Credentials, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")

	return postOAuthV2Access(ctx, httpClient, apiBase, clientID, form, Credentials{}, now)
}

// RefreshAccessToken exchanges a refresh_token for fresh user credentials.
func RefreshAccessToken(ctx context.Context, httpClient *http.Client, apiBase string, current Credentials, now time.Time) (Credentials, error) {
	if strings.TrimSpace(current.ClientID) == "" || strings.TrimSpace(current.RefreshToken) == "" {
		return Credentials{}, errors.New("stored credentials do not support token refresh; run 'slack-fetcher auth login'")
	}

	form := url.Values{}
	form.Set("client_id", current.ClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", current.RefreshToken)

	return postOAuthV2Access(ctx, httpClient, apiBase, current.ClientID, form, current, now)
}

func postOAuthV2Access(ctx context.Context, httpClient *http.Client, apiBase, clientID string, form url.Values, prev Credentials, now time.Time) (Credentials, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if strings.TrimSpace(apiBase) == "" {
		apiBase = DefaultSlackAPIBase
	}
	if now.IsZero() {
		now = time.Now()
	}

	endpoint := strings.TrimRight(apiBase, "/") + "/oauth.v2.access"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("oauth.v2.access request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Credentials{}, fmt.Errorf("oauth.v2.access HTTP %d", resp.StatusCode)
	}

	var out oauthAccessResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Credentials{}, fmt.Errorf("decode oauth.v2.access response: %w", err)
	}
	if !out.OK {
		return Credentials{}, fmt.Errorf("oauth.v2.access failed: %s", out.Error)
	}
	if strings.TrimSpace(out.AuthedUser.AccessToken) == "" {
		return Credentials{}, errors.New("oauth.v2.access response did not include authed_user.access_token (ensure user_scope is requested)")
	}

	creds := Credentials{
		AccessToken:  out.AuthedUser.AccessToken,
		RefreshToken: out.AuthedUser.RefreshToken,
		ClientID:     clientID,
		TeamID:       out.Team.ID,
		TeamName:     out.Team.Name,
		UserID:       out.AuthedUser.ID,
		Scope:        out.AuthedUser.Scope,
	}
	if out.AuthedUser.ExpiresIn > 0 {
		creds.ExpiresAt = now.Add(time.Duration(out.AuthedUser.ExpiresIn) * time.Second)
	}
	if creds.RefreshToken == "" {
		creds.RefreshToken = prev.RefreshToken
	}
	if creds.TeamID == "" {
		creds.TeamID = prev.TeamID
	}
	if creds.TeamName == "" {
		creds.TeamName = prev.TeamName
	}
	if creds.UserID == "" {
		creds.UserID = prev.UserID
	}
	if creds.Scope == "" {
		creds.Scope = prev.Scope
	}
	return creds, nil
}
