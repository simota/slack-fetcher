package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/shingoimota/slack-fetcher/internal/auth"
)

func runAuth(args []string, d deps, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printAuthUsage(stderr)
		return exitUsage
	}

	sub := args[0]
	switch sub {
	case "-h", "--help", "help":
		printAuthUsage(stdout)
		return exitOK
	case "login":
		return runAuthLogin(args[1:], d, stdout, stderr)
	case "status":
		return runAuthStatus(args[1:], d, stdout, stderr)
	case "logout":
		return runAuthLogout(args[1:], d, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "error: unknown auth subcommand %q\n", sub)
		printAuthUsage(stderr)
		return exitUsage
	}
}

func printAuthUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: slack-fetcher auth <subcommand> [flags]")
	fmt.Fprintln(w, "\nSubcommands:")
	fmt.Fprintln(w, "  login   Authenticate via Slack OAuth PKCE or --token-stdin and store credentials")
	fmt.Fprintln(w, "  status  Show the active credential source and metadata (token masked)")
	fmt.Fprintln(w, "  logout  Remove stored credentials from the local credential store")
}

func runAuthLogin(args []string, d deps, stdout, stderr io.Writer) int {
	var (
		clientID   string
		port       int
		userScope  string
		noBrowser  bool
		tokenStdin bool
		timeout    time.Duration
	)

	fs := flag.NewFlagSet("slack-fetcher auth login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&clientID, "client-id", "", "Slack App Client ID (default: SLACK_CLIENT_ID or build-time DefaultClientID)")
	fs.IntVar(&port, "port", auth.DefaultRedirectPort, "local OAuth callback port")
	fs.StringVar(&userScope, "scope", auth.DefaultUserScope, "comma-separated Slack user OAuth scopes")
	fs.BoolVar(&noBrowser, "no-browser", false, "print the authorization URL without opening a browser")
	fs.BoolVar(&tokenStdin, "token-stdin", false, "read a raw Slack token from stdin instead of running browser OAuth")
	fs.DurationVar(&timeout, "timeout", 3*time.Minute, "login timeout")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "error: unexpected positional arguments for 'auth login'")
		return exitUsage
	}
	if timeout <= 0 {
		fmt.Fprintln(stderr, "error: --timeout must be positive")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	store := resolveAuthStore(d)

	if tokenStdin {
		stdin := d.stdin
		if stdin == nil {
			stdin = os.Stdin
		}
		raw, err := io.ReadAll(io.LimitReader(stdin, 8192))
		if err != nil {
			fmt.Fprintf(stderr, "error: read token from stdin: %v\n", err)
			return exitFetchFail
		}
		token := strings.TrimSpace(string(raw))
		if token == "" {
			fmt.Fprintln(stderr, "error: no token provided on stdin")
			return exitUsage
		}
		creds := auth.Credentials{AccessToken: token}
		if err := store.Save(ctx, creds); err != nil {
			fmt.Fprintf(stderr, "error: save credentials: %v\n", err)
			return exitFetchFail
		}
		fmt.Fprintf(stdout, "Saved token (%s) to %s\n", auth.MaskToken(token), store.Description())
		return exitOK
	}

	resolvedClientID := strings.TrimSpace(clientID)
	if resolvedClientID == "" && d.getenv != nil {
		resolvedClientID = strings.TrimSpace(d.getenv("SLACK_CLIENT_ID"))
	}
	if resolvedClientID == "" {
		resolvedClientID = strings.TrimSpace(auth.DefaultClientID)
	}
	if resolvedClientID == "" {
		fmt.Fprintln(stderr, "error: Slack Client ID is not configured")
		fmt.Fprintln(stderr, "pass --client-id <id>, set SLACK_CLIENT_ID, or use --token-stdin for manual tokens")
		return exitUsage
	}
	if port < 0 || port > 65535 {
		fmt.Fprintln(stderr, "error: --port must be between 0 and 65535")
		return exitUsage
	}

	pkce, err := auth.GeneratePKCE()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitFetchFail
	}

	openFn := d.openBrowser
	if openFn == nil {
		openFn = openSystemBrowser
	}

	var usedRedirectURI string
	code, err := auth.WaitForOAuthCallback(ctx, port, pkce.State, func(redirectURI string) {
		usedRedirectURI = redirectURI
		authURL := auth.BuildAuthorizeURL(d.authorizeURL, resolvedClientID, userScope, redirectURI, pkce)
		fmt.Fprintf(stderr, "Open this URL in your browser to authorize slack-fetcher:\n%s\n", authURL)
		if !noBrowser {
			if openErr := openFn(authURL); openErr != nil {
				fmt.Fprintf(stderr, "note: could not launch browser automatically (%v); open the URL above manually\n", openErr)
			}
		}
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: OAuth login failed: %v\n", err)
		return exitAuth
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	if d.transport != nil {
		httpClient.Transport = d.transport
	}

	creds, err := auth.ExchangeCode(ctx, httpClient, d.baseURL, resolvedClientID, code, pkce.Verifier, usedRedirectURI, d.now())
	if err != nil {
		fmt.Fprintf(stderr, "error: OAuth token exchange failed: %v\n", err)
		return exitAuth
	}

	if err := store.Save(ctx, creds); err != nil {
		fmt.Fprintf(stderr, "error: save credentials: %v\n", err)
		return exitFetchFail
	}

	teamLabel := creds.TeamName
	if teamLabel == "" {
		teamLabel = creds.TeamID
	}
	if teamLabel == "" {
		teamLabel = "workspace"
	}
	fmt.Fprintf(stdout, "Logged in to %s (user=%s, token=%s) -> %s\n",
		teamLabel, creds.UserID, auth.MaskToken(creds.AccessToken), store.Description())
	return exitOK
}

func runAuthStatus(args []string, d deps, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slack-fetcher auth status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "error: unexpected positional arguments for 'auth status'")
		return exitUsage
	}

	resolver := auth.DefaultResolver(d.getenv)
	if d.authStore != nil {
		resolver.Store = d.authStore
	}
	if d.now != nil {
		resolver.Now = d.now
	}

	status, err := resolver.Status(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "Not authenticated (%s): %v\n", status.StoreDetails, err)
		return exitAuth
	}

	fmt.Fprintf(stdout, "Source: %s\n", status.Source)
	fmt.Fprintf(stdout, "Token: %s\n", status.MaskedToken)
	if status.TeamName != "" || status.TeamID != "" {
		fmt.Fprintf(stdout, "Workspace: %s (%s)\n", status.TeamName, status.TeamID)
	}
	if status.UserID != "" {
		fmt.Fprintf(stdout, "User ID: %s\n", status.UserID)
	}
	if !status.ExpiresAt.IsZero() {
		fmt.Fprintf(stdout, "Expires At: %s (refreshable=%t)\n", status.ExpiresAt.Format(time.RFC3339), status.Refreshable)
	}
	return exitOK
}

func runAuthLogout(args []string, d deps, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("slack-fetcher auth logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "error: unexpected positional arguments for 'auth logout'")
		return exitUsage
	}

	store := resolveAuthStore(d)
	if err := store.Delete(context.Background()); err != nil {
		fmt.Fprintf(stderr, "error: logout failed: %v\n", err)
		return exitFetchFail
	}
	fmt.Fprintf(stdout, "Removed stored credentials from %s\n", store.Description())
	return exitOK
}

func resolveAuthStore(d deps) auth.Store {
	if d.authStore != nil {
		return d.authStore
	}
	return auth.DefaultStore(d.getenv)
}

func openSystemBrowser(targetURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	default:
		cmd = exec.Command("xdg-open", targetURL)
	}
	return cmd.Start()
}
