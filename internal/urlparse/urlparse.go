package urlparse

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ThreadRef represents the parsed Slack channel and thread coordinates.
type ThreadRef struct {
	SourceURL      string
	WorkspaceHost  string
	ChannelID      string
	ThreadTS       string
	FocusMessageTS string
}

var (
	channelIDPattern = regexp.MustCompile(`^[CDG][A-Z0-9]{8,}$`)
	pTimestampPattern = regexp.MustCompile(`^p(\d{10})(\d{6})$`)
	dotTSPattern     = regexp.MustCompile(`^\d{10}\.\d{6}$`)
)

// ParseThreadURL parses a Slack message or thread permalink into a ThreadRef.
// Supported forms:
//   - https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16-digit-ts>
//   - https://<workspace>.slack.com/archives/<CHANNEL_ID>/p<16-digit-ts>?thread_ts=<ts>&cid=<CHANNEL_ID>
//   - https://app.slack.com/client/<TEAM_ID>/<CHANNEL_ID>/thread/<CHANNEL_ID>-<ts>
func ParseThreadURL(raw string) (ThreadRef, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ThreadRef{}, errors.New("slack thread URL is empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return ThreadRef{}, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" {
		return ThreadRef{}, fmt.Errorf("unsupported URL scheme %q: must be https", u.Scheme)
	}

	host := strings.ToLower(u.Hostname())
	if !isSlackHost(host) {
		return ThreadRef{}, fmt.Errorf("unsupported host %q: must be a slack.com domain", host)
	}

	segments := splitPath(u.Path)

	if len(segments) == 3 && segments[0] == "archives" {
		return parseArchivesURL(trimmed, host, segments, u.Query())
	}

	if len(segments) == 5 && segments[0] == "client" && segments[3] == "thread" {
		return parseClientThreadURL(trimmed, host, segments)
	}

	return ThreadRef{}, fmt.Errorf("unrecognized Slack thread URL path %q", u.Path)
}

func parseArchivesURL(raw, host string, segments []string, query url.Values) (ThreadRef, error) {
	channelID := segments[1]
	if !channelIDPattern.MatchString(channelID) {
		return ThreadRef{}, fmt.Errorf("invalid channel ID %q in URL", channelID)
	}

	msgTS, err := parsePTimestamp(segments[2])
	if err != nil {
		return ThreadRef{}, err
	}

	threadTS := msgTS
	focusTS := ""

	if qThreadTS := strings.TrimSpace(query.Get("thread_ts")); qThreadTS != "" {
		if !dotTSPattern.MatchString(qThreadTS) {
			return ThreadRef{}, fmt.Errorf("invalid thread_ts query parameter %q", qThreadTS)
		}
		threadTS = qThreadTS
		if msgTS != qThreadTS {
			focusTS = msgTS
		}
	}

	return ThreadRef{
		SourceURL:      raw,
		WorkspaceHost:  host,
		ChannelID:      channelID,
		ThreadTS:       threadTS,
		FocusMessageTS: focusTS,
	}, nil
}

func parseClientThreadURL(raw, host string, segments []string) (ThreadRef, error) {
	channelID := segments[2]
	if !channelIDPattern.MatchString(channelID) {
		return ThreadRef{}, fmt.Errorf("invalid channel ID %q in client URL", channelID)
	}

	threadSpec := segments[4]
	parts := strings.SplitN(threadSpec, "-", 2)
	if len(parts) != 2 {
		return ThreadRef{}, fmt.Errorf("invalid thread segment %q in client URL", threadSpec)
	}
	if !channelIDPattern.MatchString(parts[0]) {
		return ThreadRef{}, fmt.Errorf("invalid channel ID %q in thread segment", parts[0])
	}
	if !dotTSPattern.MatchString(parts[1]) {
		return ThreadRef{}, fmt.Errorf("invalid thread timestamp %q in client URL", parts[1])
	}

	return ThreadRef{
		SourceURL:     raw,
		WorkspaceHost: host,
		ChannelID:     parts[0],
		ThreadTS:      parts[1],
	}, nil
}

func parsePTimestamp(seg string) (string, error) {
	m := pTimestampPattern.FindStringSubmatch(seg)
	if m == nil {
		return "", fmt.Errorf("invalid message timestamp segment %q: expected p followed by 16 digits", seg)
	}
	return m[1] + "." + m[2], nil
}

func splitPath(p string) []string {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func isSlackHost(host string) bool {
	return host == "slack.com" || strings.HasSuffix(host, ".slack.com")
}
