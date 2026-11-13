package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultBaseURL    = "https://slack.com/api"
	maxRateLimitRetry = 3
	maxRedirects      = 10
)

var (
	ErrFileTooLarge           = errors.New("file exceeds maximum download size")
	ErrUnexpectedHTMLResponse = errors.New("received text/html response instead of file content (authentication may have redirected to login)")
	ErrUntrustedDownloadURL   = errors.New("untrusted file download URL")
)

// APIError represents a structured error returned by Slack Web API.
type APIError struct {
	Method     string
	Code       string
	Needed     string
	Provided   string
	StatusCode int
	Detail     string
}

func (e *APIError) Error() string {
	var parts []string
	if e.Method != "" {
		parts = append(parts, e.Method)
	}
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Needed != "" {
		parts = append(parts, fmt.Sprintf("needed=%s", e.Needed))
	}
	if e.StatusCode != 0 && e.Code == "" {
		parts = append(parts, fmt.Sprintf("http_status=%d", e.StatusCode))
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	return "slack api error: " + strings.Join(parts, ": ")
}

// IsAuthOrPermission reports whether the error is due to authentication or missing permissions.
func (e *APIError) IsAuthOrPermission() bool {
	switch e.Code {
	case "not_authed", "invalid_auth", "account_inactive", "token_revoked",
		"token_expired", "missing_scope", "not_in_channel", "ekm_access_denied", "no_permission":
		return true
	default:
		return false
	}
}

// IsNotFound reports whether the requested channel, thread, or message does not exist.
func (e *APIError) IsNotFound() bool {
	switch e.Code {
	case "thread_not_found", "channel_not_found", "message_not_found":
		return true
	default:
		return false
	}
}

type RawMessage struct {
	Type        string            `json:"type"`
	Subtype     string            `json:"subtype"`
	User        string            `json:"user"`
	BotID       string            `json:"bot_id"`
	Username    string            `json:"username"`
	BotProfile  *BotProfile       `json:"bot_profile,omitempty"`
	Text        string            `json:"text"`
	TS          string            `json:"ts"`
	ThreadTS    string            `json:"thread_ts"`
	Attachments []RawAttachment   `json:"attachments"`
	Blocks      []json.RawMessage `json:"blocks"`
	Files       []RawFile         `json:"files"`
}

type BotProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type RawAttachment struct {
	Pretext    string               `json:"pretext"`
	AuthorName string               `json:"author_name"`
	Title      string               `json:"title"`
	TitleLink  string               `json:"title_link"`
	Text       string               `json:"text"`
	Fallback   string               `json:"fallback"`
	Fields     []RawAttachmentField `json:"fields"`
	Footer     string               `json:"footer"`
}

type RawAttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

type RawFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Title              string `json:"title"`
	Mimetype           string `json:"mimetype"`
	Filetype           string `json:"filetype"`
	Size               int64  `json:"size"`
	Mode               string `json:"mode"`
	URLPrivate         string `json:"url_private"`
	URLPrivateDownload string `json:"url_private_download"`
	Permalink          string `json:"permalink"`
}

type Author struct {
	ID          string
	DisplayName string
	RealName    string
	Name        string
}

// PreferredName returns the most human-readable handle available for the user.
func (a Author) PreferredName() string {
	switch {
	case strings.TrimSpace(a.DisplayName) != "":
		return strings.TrimSpace(a.DisplayName)
	case strings.TrimSpace(a.RealName) != "":
		return strings.TrimSpace(a.RealName)
	case strings.TrimSpace(a.Name) != "":
		return strings.TrimSpace(a.Name)
	default:
		return a.ID
	}
}

type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
	userCache  map[string]Author
	sleep      func(context.Context, time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithTransport injects a custom http.RoundTripper (primarily for tests).
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) {
		if rt != nil {
			c.httpClient.Transport = rt
		}
	}
}

// WithBaseURL overrides the Slack Web API base URL.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if strings.TrimSpace(baseURL) != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

func withSleep(fn func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if fn != nil {
			c.sleep = fn
		}
	}
}

// NewClient constructs a Slack Web API client with redirect token isolation.
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		token:     token,
		baseURL:   defaultBaseURL,
		userCache: make(map[string]Author),
		sleep:     sleepWithContext,
	}
	c.httpClient = &http.Client{
		CheckRedirect: c.checkRedirect,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS redirect to %s", req.URL.Redacted())
	}
	if isTrustedSlackFileHost(req.URL.Hostname()) {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else {
		// Prevent leaking the bearer token to pre-signed external storage (e.g. S3).
		req.Header.Del("Authorization")
	}
	return nil
}

// FetchReplies retrieves all messages in a thread using cursor pagination.
func (c *Client) FetchReplies(ctx context.Context, channelID, threadTS string) ([]RawMessage, error) {
	var all []RawMessage
	cursor := ""

	for {
		q := url.Values{}
		q.Set("channel", channelID)
		q.Set("ts", threadTS)
		q.Set("limit", "200")
		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var resp struct {
			OK               bool         `json:"ok"`
			Error            string       `json:"error"`
			Needed           string       `json:"needed"`
			Provided         string       `json:"provided"`
			Messages         []RawMessage `json:"messages"`
			HasMore          bool         `json:"has_more"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}

		if err := c.callAPI(ctx, "conversations.replies", q, &resp); err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, &APIError{
				Method:   "conversations.replies",
				Code:     resp.Error,
				Needed:   resp.Needed,
				Provided: resp.Provided,
			}
		}

		all = append(all, resp.Messages...)
		next := strings.TrimSpace(resp.ResponseMetadata.NextCursor)
		if next == "" {
			break
		}
		cursor = next
	}

	return all, nil
}

// ResolveUsers resolves unique user IDs via users.info with in-memory caching.
// Individual non-auth lookup failures are returned as non-fatal warnings, while
// auth/permission errors and context cancellations fail immediately.
func (c *Client) ResolveUsers(ctx context.Context, userIDs []string) (map[string]Author, []error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	result := make(map[string]Author, len(userIDs))
	var warnings []error

	for _, id := range userIDs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if cached, ok := c.userCache[id]; ok {
			result[id] = cached
			continue
		}

		q := url.Values{}
		q.Set("user", id)
		var resp struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Needed   string `json:"needed"`
			Provided string `json:"provided"`
			User     struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				RealName string `json:"real_name"`
				Profile  struct {
					DisplayName string `json:"display_name"`
					RealName    string `json:"real_name"`
				} `json:"profile"`
			} `json:"user"`
		}

		if err := c.callAPI(ctx, "users.info", q, &resp); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, err
			}
			warnings = append(warnings, fmt.Errorf("users.info(%s): %w", id, err))
			continue
		}
		if !resp.OK {
			apiErr := &APIError{
				Method:   "users.info",
				Code:     resp.Error,
				Needed:   resp.Needed,
				Provided: resp.Provided,
				Detail:   "user=" + id,
			}
			if apiErr.IsAuthOrPermission() {
				return nil, nil, apiErr
			}
			warnings = append(warnings, apiErr)
			continue
		}

		realName := resp.User.Profile.RealName
		if realName == "" {
			realName = resp.User.RealName
		}
		author := Author{
			ID:          id,
			DisplayName: resp.User.Profile.DisplayName,
			RealName:    realName,
			Name:        resp.User.Name,
		}
		c.userCache[id] = author
		result[id] = author
	}

	return result, warnings, nil
}

// FetchPrivateFile downloads a Slack private file once.
// If maxInlineBytes >= 0 and the payload fits within maxInlineBytes and is valid UTF-8,
// it returns inline bytes without touching disk (saved=false).
// Otherwise, it streams the content to destPath up to maxDownloadBytes (saved=true).
func (c *Client) FetchPrivateFile(ctx context.Context, downloadURL string, maxInlineBytes, maxDownloadBytes int64, destPath string) (inline []byte, written int64, saved bool, err error) {
	u, err := url.Parse(strings.TrimSpace(downloadURL))
	if err != nil {
		return nil, 0, false, fmt.Errorf("%w: %v", ErrUntrustedDownloadURL, err)
	}
	if u.Scheme != "https" || !isTrustedSlackFileHost(u.Hostname()) {
		return nil, 0, false, fmt.Errorf("%w: %s", ErrUntrustedDownloadURL, u.Redacted())
	}

	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		return req, nil
	})
	if err != nil {
		return nil, 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, false, &APIError{
			Method:     "download",
			StatusCode: resp.StatusCode,
			Detail:     resp.Status,
		}
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		mediaType, _, _ := mime.ParseMediaType(ct)
		if strings.EqualFold(mediaType, "text/html") {
			return nil, 0, false, ErrUnexpectedHTMLResponse
		}
	}

	var prefix []byte
	var reader io.Reader = resp.Body

	if maxInlineBytes >= 0 {
		headLimit := maxInlineBytes + 1
		if maxDownloadBytes >= 0 && headLimit > maxDownloadBytes+1 {
			headLimit = maxDownloadBytes + 1
		}
		head, err := io.ReadAll(io.LimitReader(resp.Body, headLimit))
		if err != nil {
			return nil, 0, false, fmt.Errorf("read response: %w", err)
		}
		if maxDownloadBytes >= 0 && int64(len(head)) > maxDownloadBytes {
			return nil, 0, false, ErrFileTooLarge
		}
		if int64(len(head)) <= maxInlineBytes && utf8.Valid(head) {
			return head, int64(len(head)), false, nil
		}
		prefix = head
		reader = io.MultiReader(bytes.NewReader(prefix), resp.Body)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, 0, false, fmt.Errorf("create attachment directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".slack-fetcher-*")
	if err != nil {
		return nil, 0, false, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	var n int64
	if maxDownloadBytes >= 0 {
		n, err = io.Copy(tmp, io.LimitReader(reader, maxDownloadBytes+1))
		if err != nil {
			return nil, 0, false, fmt.Errorf("write attachment: %w", err)
		}
		if n > maxDownloadBytes {
			return nil, 0, false, ErrFileTooLarge
		}
	} else {
		n, err = io.Copy(tmp, reader)
		if err != nil {
			return nil, 0, false, fmt.Errorf("write attachment: %w", err)
		}
	}

	if err := tmp.Close(); err != nil {
		return nil, 0, false, fmt.Errorf("close attachment: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return nil, 0, false, fmt.Errorf("finalize attachment: %w", err)
	}
	cleanup = false
	return nil, n, true, nil
}

func (c *Client) callAPI(ctx context.Context, method string, query url.Values, out any) error {
	endpoint := c.baseURL + "/" + method
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{
			Method:     method,
			StatusCode: resp.StatusCode,
			Detail:     resp.Status,
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode response: %w", method, err)
	}
	return nil
}

func (c *Client) doWithRetry(ctx context.Context, makeReq func() (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; attempt <= maxRateLimitRetry; attempt++ {
		req, err := makeReq()
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		wait := parseRetryAfter(resp.Header.Get("Retry-After"))
		_ = resp.Body.Close()

		if attempt == maxRateLimitRetry {
			return nil, &APIError{
				StatusCode: http.StatusTooManyRequests,
				Detail:     fmt.Sprintf("rate limited after %d retries", maxRateLimitRetry),
			}
		}

		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("unreachable retry loop")
}

func parseRetryAfter(raw string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || secs < 0 {
		return time.Second
	}
	return time.Duration(secs) * time.Second
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isTrustedSlackFileHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	for _, domain := range []string{"slack.com", "slack-edge.com", "slack-files.com"} {
		if h == domain || strings.HasSuffix(h, "."+domain) {
			return true
		}
	}
	return false
}
