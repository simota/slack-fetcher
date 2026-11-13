package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shingoimota/slack-fetcher/internal/auth"
	"github.com/shingoimota/slack-fetcher/internal/normalizer"
	"github.com/shingoimota/slack-fetcher/internal/renderer"
	"github.com/shingoimota/slack-fetcher/internal/slackapi"
	"github.com/shingoimota/slack-fetcher/internal/urlparse"
)

const (
	exitOK        = 0
	exitUsage     = 2
	exitAuth      = 3
	exitFetchFail = 4
)

type deps struct {
	getenv       func(string) string
	stdin        io.Reader
	transport    http.RoundTripper
	baseURL      string
	authorizeURL string
	tokenFn      func(context.Context) (string, error)
	authStore    auth.Store
	openBrowser  func(string) error
	now          func() time.Time
}

func main() {
	d := deps{
		getenv: os.Getenv,
		now:    time.Now,
	}
	os.Exit(run(os.Args[1:], d, os.Stdout, os.Stderr))
}

func run(args []string, d deps, stdout, stderr io.Writer) int {
	if d.getenv == nil {
		d.getenv = os.Getenv
	}
	if d.now == nil {
		d.now = time.Now
	}

	if len(args) > 0 && args[0] == "auth" {
		return runAuth(args[1:], d, stdout, stderr)
	}

	var (
		attachmentDir    string
		outputPath       string
		maxInlineBytes   int64
		maxDownloadBytes int64
		timeout          time.Duration
	)

	fs := flag.NewFlagSet("slack-fetcher", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&attachmentDir, "attachment-dir", "", "directory to save binary attachments")
	fs.StringVar(&outputPath, "output", "", "output file path (default stdout)")
	fs.StringVar(&outputPath, "o", "", "output file path (shorthand)")
	fs.Int64Var(&maxInlineBytes, "max-inline-bytes", normalizer.DefaultMaxInlineBytes, "max bytes for inline text attachments")
	fs.Int64Var(&maxDownloadBytes, "max-download-bytes", normalizer.DefaultMaxDownloadBytes, "max bytes per downloaded attachment")
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "overall command timeout")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage:")
		fmt.Fprintln(stderr, "  slack-fetcher [flags] <thread-url>")
		fmt.Fprintln(stderr, "  slack-fetcher auth <login|status|logout> [flags]")
		fmt.Fprintln(stderr, "\nSupported URLs:")
		fmt.Fprintln(stderr, "  https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16-digit-ts>")
		fmt.Fprintln(stderr, "  https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16-digit-ts>?thread_ts=<ts>")
		fmt.Fprintln(stderr, "  https://app.slack.com/client/<TEAM_ID>/<CHANNEL_ID>/thread/<CHANNEL_ID>-<ts>")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "error: exactly one <thread-url> argument is required")
		fs.Usage()
		return exitUsage
	}
	if maxInlineBytes <= 0 {
		fmt.Fprintln(stderr, "error: --max-inline-bytes must be positive")
		return exitUsage
	}
	if maxDownloadBytes <= 0 {
		fmt.Fprintln(stderr, "error: --max-download-bytes must be positive")
		return exitUsage
	}
	if timeout <= 0 {
		fmt.Fprintln(stderr, "error: --timeout must be positive")
		return exitUsage
	}

	ref, err := urlparse.ParseThreadURL(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "error: invalid Slack thread URL: %v\n", err)
		fmt.Fprintln(stderr, "example: https://example.slack.com/archives/C01234567/p1700000000123456")
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var token string
	if d.tokenFn != nil {
		token, err = d.tokenFn(ctx)
	} else {
		resolver := auth.DefaultResolver(d.getenv)
		if d.authStore != nil {
			resolver.Store = d.authStore
		}
		if d.transport != nil {
			resolver.HTTPClient = &http.Client{Transport: d.transport, Timeout: 30 * time.Second}
		}
		if d.baseURL != "" {
			resolver.OAuthBaseURL = d.baseURL
		}
		resolver.Now = d.now
		token, err = resolver.ResolveToken(ctx)
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return exitFetchFail
		}
		return exitAuth
	}

	var clientOpts []slackapi.Option
	if d.transport != nil {
		clientOpts = append(clientOpts, slackapi.WithTransport(d.transport))
	}
	if d.baseURL != "" {
		clientOpts = append(clientOpts, slackapi.WithBaseURL(d.baseURL))
	}
	client := slackapi.NewClient(token, clientOpts...)

	rawMsgs, err := client.FetchReplies(ctx, ref.ChannelID, ref.ThreadTS)
	if err != nil {
		return reportFetchError(stderr, err)
	}
	if len(rawMsgs) == 0 {
		fmt.Fprintf(stderr, "error: thread not found (%s / %s)\n", ref.ChannelID, ref.ThreadTS)
		return exitFetchFail
	}

	if strings.TrimSpace(attachmentDir) == "" {
		attachmentDir = filepath.Join(os.TempDir(), "slack-fetcher", fmt.Sprintf("%s_%s", ref.ChannelID, ref.ThreadTS))
	}

	doc, err := normalizer.NormalizeThread(ctx, ref, rawMsgs, normalizer.NormalizeOptions{
		AttachmentDir:    attachmentDir,
		MaxInlineBytes:   maxInlineBytes,
		MaxDownloadBytes: maxDownloadBytes,
		FetchedAt:        d.now(),
		Client:           client,
	})
	if err != nil {
		return reportFetchError(stderr, err)
	}
	if err := ctx.Err(); err != nil {
		return reportFetchError(stderr, err)
	}

	for _, w := range doc.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}

	var rendered bytes.Buffer
	if err := renderer.RenderMarkdown(&rendered, doc); err != nil {
		fmt.Fprintf(stderr, "error: render markdown: %v\n", err)
		return exitFetchFail
	}

	if strings.TrimSpace(outputPath) != "" {
		if err := writeOutputFile(outputPath, rendered.Bytes()); err != nil {
			fmt.Fprintf(stderr, "error: write output file: %v\n", err)
			return exitFetchFail
		}
		return exitOK
	}

	if _, err := stdout.Write(rendered.Bytes()); err != nil {
		fmt.Fprintf(stderr, "error: write stdout: %v\n", err)
		return exitFetchFail
	}
	return exitOK
}

func reportFetchError(stderr io.Writer, err error) int {
	var apiErr *slackapi.APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsAuthOrPermission() {
			switch apiErr.Code {
			case "missing_scope":
				fmt.Fprintf(stderr, "error: missing Slack OAuth scope (needed=%s, provided=%s)\n", apiErr.Needed, apiErr.Provided)
			case "not_in_channel":
				fmt.Fprintln(stderr, "error: token is not in the target channel (use a User OAuth Token xoxp- or /invite the bot)")
			case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive":
				fmt.Fprintf(stderr, "error: Slack authentication failed (%s); run 'slack-fetcher auth login' or verify SLACK_TOKEN\n", apiErr.Code)
			default:
				fmt.Fprintf(stderr, "error: Slack permission error: %v\n", apiErr)
			}
			return exitAuth
		}
		if apiErr.IsNotFound() {
			fmt.Fprintf(stderr, "error: Slack thread or channel not found (%s)\n", apiErr.Code)
			return exitFetchFail
		}
	}
	fmt.Fprintf(stderr, "error: fetch thread failed: %v\n", err)
	return exitFetchFail
}

func writeOutputFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}
