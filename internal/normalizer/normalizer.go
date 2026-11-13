package normalizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/shingoimota/slack-fetcher/internal/slackapi"
	"github.com/shingoimota/slack-fetcher/internal/urlparse"
)

const (
	DefaultMaxInlineBytes   int64 = 256 * 1024
	DefaultMaxDownloadBytes int64 = 50 * 1024 * 1024
)

type AttachmentKind string

const (
	AttachmentInline      AttachmentKind = "inline"
	AttachmentSaved       AttachmentKind = "saved"
	AttachmentUnavailable AttachmentKind = "unavailable"
	AttachmentExternal    AttachmentKind = "external"
	AttachmentSkipped     AttachmentKind = "skipped"
)

type Attachment struct {
	Kind      AttachmentKind
	Name      string
	MIMEType  string
	Size      int64
	Language  string
	Content   string
	Path      string
	Permalink string
	Reason    string
}

type Message struct {
	Index       int
	Timestamp   time.Time
	TS          string
	IsFocused   bool
	AuthorLabel string
	Body        string
	Attachments []Attachment
}

type ThreadDocument struct {
	SourceURL      string
	ChannelID      string
	ThreadTS       string
	FocusMessageTS string
	FetchedAt      time.Time
	Participants   []string
	AttachmentDir  string
	Messages       []Message
	Warnings       []string
}

type NormalizeOptions struct {
	AttachmentDir    string
	MaxInlineBytes   int64
	MaxDownloadBytes int64
	FetchedAt        time.Time
	Location         *time.Location
	Client           *slackapi.Client
}

var (
	slackTokenPattern = regexp.MustCompile(`<([^<>]+)>`)
	userMentionInText = regexp.MustCompile(`<@([UW][A-Z0-9]+)(?:\|[^<>]*)?>`)
)

// NormalizeThread resolves users, expands Slack mrkdwn, extracts bot attachments/blocks,
// and fetches/classifies attached files into inline text or locally saved binaries.
func NormalizeThread(ctx context.Context, ref urlparse.ThreadRef, rawMsgs []slackapi.RawMessage, opts NormalizeOptions) (ThreadDocument, error) {
	if opts.MaxInlineBytes <= 0 {
		opts.MaxInlineBytes = DefaultMaxInlineBytes
	}
	if opts.MaxDownloadBytes <= 0 {
		opts.MaxDownloadBytes = DefaultMaxDownloadBytes
	}
	fetchedAt := opts.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = time.Now()
	}
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	fetchedAt = fetchedAt.In(loc)

	absAttachmentDir := ""
	if strings.TrimSpace(opts.AttachmentDir) != "" {
		abs, err := filepath.Abs(opts.AttachmentDir)
		if err != nil {
			return ThreadDocument{}, fmt.Errorf("resolve attachment-dir: %w", err)
		}
		absAttachmentDir = abs
	}

	sortedMsgs := deduplicateAndSortMessages(rawMsgs)

	if err := ctx.Err(); err != nil {
		return ThreadDocument{}, err
	}

	var warnings []string
	users := make(map[string]slackapi.Author)
	if opts.Client != nil {
		userIDs := collectUserIDs(sortedMsgs)
		if len(userIDs) > 0 {
			var userWarnings []error
			var err error
			users, userWarnings, err = opts.Client.ResolveUsers(ctx, userIDs)
			if err != nil {
				return ThreadDocument{}, err
			}
			for _, w := range userWarnings {
				warnings = append(warnings, w.Error())
			}
		}
	}

	doc := ThreadDocument{
		SourceURL:      ref.SourceURL,
		ChannelID:      ref.ChannelID,
		ThreadTS:       ref.ThreadTS,
		FocusMessageTS: ref.FocusMessageTS,
		FetchedAt:      fetchedAt,
	}

	seenParticipants := make(map[string]struct{})
	var participants []string
	addParticipant := func(label string) {
		if label == "" {
			return
		}
		if _, ok := seenParticipants[label]; !ok {
			seenParticipants[label] = struct{}{}
			participants = append(participants, label)
		}
	}

	fileSeq := 0
	savedAny := false

	for i, raw := range sortedMsgs {
		authorLabel, participantEntry := resolveAuthor(raw, users)
		addParticipant(participantEntry)

		body := buildMessageBody(raw, users)

		var attachments []Attachment
		for _, rf := range raw.Files {
			fileSeq++
			att, saved, warn, err := processFile(ctx, opts, absAttachmentDir, fileSeq, rf)
			if err != nil {
				return ThreadDocument{}, err
			}
			if saved {
				savedAny = true
			}
			if warn != "" {
				warnings = append(warnings, warn)
			}
			attachments = append(attachments, att)
		}

		doc.Messages = append(doc.Messages, Message{
			Index:       i + 1,
			Timestamp:   parseSlackTS(raw.TS, loc),
			TS:          raw.TS,
			IsFocused:   ref.FocusMessageTS != "" && raw.TS == ref.FocusMessageTS,
			AuthorLabel: authorLabel,
			Body:        body,
			Attachments: attachments,
		})
	}

	doc.Participants = participants
	if savedAny {
		doc.AttachmentDir = absAttachmentDir
	}
	doc.Warnings = warnings
	return doc, nil
}

func deduplicateAndSortMessages(raw []slackapi.RawMessage) []slackapi.RawMessage {
	seen := make(map[string]struct{}, len(raw))
	out := make([]slackapi.RawMessage, 0, len(raw))
	for _, m := range raw {
		if m.TS != "" {
			if _, ok := seen[m.TS]; ok {
				continue
			}
			seen[m.TS] = struct{}{}
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return compareSlackTS(out[i].TS, out[j].TS) < 0
	})
	return out
}

func compareSlackTS(a, b string) int {
	aSec, aMicro := splitTS(a)
	bSec, bMicro := splitTS(b)
	if aSec != bSec {
		if aSec < bSec {
			return -1
		}
		return 1
	}
	if aMicro < bMicro {
		return -1
	}
	if aMicro > bMicro {
		return 1
	}
	return strings.Compare(a, b)
}

func splitTS(ts string) (int64, int64) {
	parts := strings.SplitN(strings.TrimSpace(ts), ".", 2)
	sec, _ := strconv.ParseInt(parts[0], 10, 64)
	var micro int64
	if len(parts) == 2 {
		frac := parts[1]
		for len(frac) < 6 {
			frac += "0"
		}
		if len(frac) > 6 {
			frac = frac[:6]
		}
		micro, _ = strconv.ParseInt(frac, 10, 64)
	}
	return sec, micro
}

func parseSlackTS(ts string, loc *time.Location) time.Time {
	sec, micro := splitTS(ts)
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, micro*1000).In(loc)
}

func collectUserIDs(msgs []slackapi.RawMessage) []string {
	seen := make(map[string]struct{})
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	scanText := func(s string) {
		for _, m := range userMentionInText.FindAllStringSubmatch(s, -1) {
			add(m[1])
		}
	}

	for _, m := range msgs {
		add(m.User)
		scanText(m.Text)
		for _, att := range m.Attachments {
			scanText(att.Pretext)
			scanText(att.AuthorName)
			scanText(att.Title)
			scanText(att.Text)
			scanText(att.Footer)
			scanText(att.Fallback)
			for _, f := range att.Fields {
				scanText(f.Title)
				scanText(f.Value)
			}
		}
		for _, b := range m.Blocks {
			scanText(string(b))
			var parsed any
			if err := json.Unmarshal(b, &parsed); err == nil {
				collectBlockUserIDs(parsed, add)
			}
		}
	}
	return ids
}

func collectBlockUserIDs(v any, add func(string)) {
	switch node := v.(type) {
	case map[string]any:
		if node["type"] == "user" {
			if uid, ok := node["user_id"].(string); ok {
				add(uid)
			}
		}
		for _, child := range node {
			collectBlockUserIDs(child, add)
		}
	case []any:
		for _, item := range node {
			collectBlockUserIDs(item, add)
		}
	}
}

func resolveAuthor(m slackapi.RawMessage, users map[string]slackapi.Author) (authorLabel, participantEntry string) {
	if m.User != "" {
		if u, ok := users[m.User]; ok {
			name := "@" + u.PreferredName()
			return name, fmt.Sprintf("%s (%s)", name, m.User)
		}
		name := "@" + m.User
		return name, fmt.Sprintf("%s (%s)", name, m.User)
	}
	botName := strings.TrimSpace(m.Username)
	if botName == "" && m.BotProfile != nil {
		botName = strings.TrimSpace(m.BotProfile.Name)
	}
	if botName == "" {
		botName = strings.TrimSpace(m.BotID)
	}
	if botName == "" {
		botName = "bot"
	}
	label := "bot:" + botName
	return label, label
}

func buildMessageBody(m slackapi.RawMessage, users map[string]slackapi.Author) string {
	var sections []string

	if text := strings.TrimSpace(expandMrkdwn(m.Text, users)); text != "" {
		sections = append(sections, text)
	} else if len(m.Attachments) == 0 && len(m.Blocks) > 0 {
		if blockText := strings.TrimSpace(extractBlocksText(m.Blocks, users)); blockText != "" {
			sections = append(sections, blockText)
		}
	}

	for _, att := range m.Attachments {
		if formatted := formatBotAttachment(att, users); formatted != "" {
			sections = append(sections, formatted)
		}
	}

	return strings.Join(sections, "\n\n")
}

func formatBotAttachment(att slackapi.RawAttachment, users map[string]slackapi.Author) string {
	var lines []string
	if s := strings.TrimSpace(expandMrkdwn(att.Pretext, users)); s != "" {
		lines = append(lines, s)
	}
	if s := strings.TrimSpace(expandMrkdwn(att.AuthorName, users)); s != "" {
		lines = append(lines, "**Author**: "+s)
	}
	if title := strings.TrimSpace(expandMrkdwn(att.Title, users)); title != "" {
		if link := strings.TrimSpace(att.TitleLink); link != "" {
			lines = append(lines, fmt.Sprintf("**[%s](%s)**", title, link))
		} else {
			lines = append(lines, fmt.Sprintf("**%s**", title))
		}
	}
	if s := strings.TrimSpace(expandMrkdwn(att.Text, users)); s != "" {
		lines = append(lines, s)
	}
	for _, f := range att.Fields {
		ft := strings.TrimSpace(expandMrkdwn(f.Title, users))
		fv := strings.TrimSpace(expandMrkdwn(f.Value, users))
		switch {
		case ft != "" && fv != "":
			lines = append(lines, fmt.Sprintf("- **%s**: %s", ft, fv))
		case fv != "":
			lines = append(lines, "- "+fv)
		case ft != "":
			lines = append(lines, "- **"+ft+"**")
		}
	}
	if s := strings.TrimSpace(expandMrkdwn(att.Footer, users)); s != "" {
		lines = append(lines, "_"+s+"_")
	}
	if len(lines) == 0 {
		if fb := strings.TrimSpace(expandMrkdwn(att.Fallback, users)); fb != "" {
			lines = append(lines, fb)
		}
	}
	return strings.Join(lines, "\n")
}

func extractBlocksText(blocks []json.RawMessage, users map[string]slackapi.Author) string {
	var lines []string
	for _, raw := range blocks {
		var node map[string]any
		if err := json.Unmarshal(raw, &node); err != nil {
			continue
		}
		switch node["type"] {
		case "header", "section":
			if txt := extractTextObject(node["text"], users); txt != "" {
				lines = append(lines, txt)
			}
			if fields, ok := node["fields"].([]any); ok {
				for _, f := range fields {
					if txt := extractTextObject(f, users); txt != "" {
						lines = append(lines, "- "+txt)
					}
				}
			}
		case "context":
			if elems, ok := node["elements"].([]any); ok {
				var parts []string
				for _, el := range elems {
					if txt := extractTextObject(el, users); txt != "" {
						parts = append(parts, txt)
					}
				}
				if len(parts) > 0 {
					lines = append(lines, strings.Join(parts, " | "))
				}
			}
		case "rich_text":
			if txt := extractRichText(node, users); txt != "" {
				lines = append(lines, txt)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func extractTextObject(v any, users map[string]slackapi.Author) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	text, _ := m["text"].(string)
	if text == "" {
		return ""
	}
	typ, _ := m["type"].(string)
	if typ == "mrkdwn" {
		return strings.TrimSpace(expandMrkdwn(text, users))
	}
	return strings.TrimSpace(text)
}

func extractRichText(node map[string]any, users map[string]slackapi.Author) string {
	elems, _ := node["elements"].([]any)
	var parts []string
	for _, rawEl := range elems {
		el, ok := rawEl.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := el["type"].(string)
		switch typ {
		case "rich_text_section", "rich_text_quote":
			s := extractRichTextInline(el, users)
			if typ == "rich_text_quote" && s != "" {
				s = "> " + strings.ReplaceAll(s, "\n", "\n> ")
			}
			if s != "" {
				parts = append(parts, s)
			}
		case "rich_text_preformatted":
			s := extractRichTextInline(el, users)
			if s != "" {
				fence := SafeMarkdownFence(s)
				parts = append(parts, fence+"\n"+s+"\n"+fence)
			}
		case "rich_text_list":
			subElems, _ := el["elements"].([]any)
			for _, sub := range subElems {
				if subMap, ok := sub.(map[string]any); ok {
					if s := extractRichTextInline(subMap, users); s != "" {
						parts = append(parts, "- "+s)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

func extractRichTextInline(node map[string]any, users map[string]slackapi.Author) string {
	elems, _ := node["elements"].([]any)
	var b strings.Builder
	for _, rawEl := range elems {
		el, ok := rawEl.(map[string]any)
		if !ok {
			continue
		}
		switch el["type"] {
		case "text":
			if s, ok := el["text"].(string); ok {
				b.WriteString(s)
			}
		case "user":
			if uid, ok := el["user_id"].(string); ok {
				if u, exists := users[uid]; exists {
					b.WriteString("@" + u.PreferredName())
				} else {
					b.WriteString("@" + uid)
				}
			}
		case "channel":
			if cid, ok := el["channel_id"].(string); ok {
				b.WriteString("#" + cid)
			}
		case "usergroup":
			if gid, ok := el["usergroup_id"].(string); ok {
				b.WriteString("@subteam:" + gid)
			}
		case "broadcast":
			if r, ok := el["range"].(string); ok {
				b.WriteString("@" + r)
			}
		case "link":
			u, _ := el["url"].(string)
			t, _ := el["text"].(string)
			if t != "" && t != u {
				fmt.Fprintf(&b, "[%s](%s)", t, u)
			} else {
				b.WriteString(u)
			}
		case "emoji":
			if n, ok := el["name"].(string); ok {
				b.WriteString(":" + n + ":")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// expandMrkdwn converts Slack angle-bracket tokens and unescapes Slack HTML entities.
func expandMrkdwn(input string, users map[string]slackapi.Author) string {
	if input == "" {
		return ""
	}
	var out strings.Builder
	last := 0
	for _, loc := range slackTokenPattern.FindAllStringSubmatchIndex(input, -1) {
		out.WriteString(unescapeSlackEntities(input[last:loc[0]]))
		token := input[loc[2]:loc[3]]
		out.WriteString(expandToken(token, users))
		last = loc[1]
	}
	out.WriteString(unescapeSlackEntities(input[last:]))
	return out.String()
}

func expandToken(token string, users map[string]slackapi.Author) string {
	body, label, hasLabel := strings.Cut(token, "|")
	label = unescapeSlackEntities(label)

	switch {
	case strings.HasPrefix(body, "@"):
		uid := body[1:]
		if u, ok := users[uid]; ok {
			return "@" + u.PreferredName()
		}
		if hasLabel && strings.TrimSpace(label) != "" {
			return "@" + strings.TrimPrefix(strings.TrimSpace(label), "@")
		}
		return "@" + uid

	case strings.HasPrefix(body, "#"):
		cid := body[1:]
		if hasLabel && strings.TrimSpace(label) != "" {
			return "#" + strings.TrimPrefix(strings.TrimSpace(label), "#")
		}
		return "#" + cid

	case strings.HasPrefix(body, "!subteam^"):
		if hasLabel && strings.TrimSpace(label) != "" {
			l := strings.TrimSpace(label)
			if strings.HasPrefix(l, "@") {
				return l
			}
			return "@" + l
		}
		return "@subteam:" + strings.TrimPrefix(body, "!subteam^")

	case strings.HasPrefix(body, "!date^"):
		if hasLabel && strings.TrimSpace(label) != "" {
			return label
		}
		return body

	case body == "!here" || body == "!channel" || body == "!everyone":
		return "@" + body[1:]

	case strings.HasPrefix(body, "!"):
		if hasLabel && strings.TrimSpace(label) != "" {
			return label
		}
		return "@" + body[1:]

	case strings.HasPrefix(body, "http://") || strings.HasPrefix(body, "https://"):
		rawURL := unescapeSlackEntities(body)
		if hasLabel && strings.TrimSpace(label) != "" && label != rawURL {
			return fmt.Sprintf("[%s](%s)", label, rawURL)
		}
		return rawURL

	case strings.HasPrefix(body, "mailto:"):
		addr := strings.TrimPrefix(unescapeSlackEntities(body), "mailto:")
		if hasLabel && strings.TrimSpace(label) != "" && label != addr {
			return fmt.Sprintf("%s (%s)", label, addr)
		}
		return addr

	default:
		if hasLabel && strings.TrimSpace(label) != "" {
			return label
		}
		return "<" + unescapeSlackEntities(token) + ">"
	}
}

func unescapeSlackEntities(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">")
	return r.Replace(s)
}

func processFile(ctx context.Context, opts NormalizeOptions, attachmentDir string, seq int, rf slackapi.RawFile) (att Attachment, saved bool, warning string, fatalErr error) {
	if err := ctx.Err(); err != nil {
		return Attachment{}, false, "", err
	}

	displayName := strings.TrimSpace(rf.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(rf.Title)
	}
	if displayName == "" {
		displayName = rf.ID
	}
	if displayName == "" {
		displayName = "attachment"
	}

	mimeType := normalizeMIME(rf.Mimetype)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	switch rf.Mode {
	case "tombstone":
		return Attachment{
			Kind:   AttachmentUnavailable,
			Name:   displayName,
			Reason: "deleted file",
		}, false, "", nil
	case "external":
		link := rf.Permalink
		if link == "" {
			link = rf.URLPrivate
		}
		return Attachment{
			Kind:      AttachmentExternal,
			Name:      displayName,
			MIMEType:  mimeType,
			Size:      rf.Size,
			Permalink: link,
		}, false, "", nil
	}

	downloadURL := strings.TrimSpace(rf.URLPrivateDownload)
	if downloadURL == "" {
		downloadURL = strings.TrimSpace(rf.URLPrivate)
	}
	if downloadURL == "" {
		msg := fmt.Sprintf("skipped %s: no download URL provided", displayName)
		return Attachment{
			Kind:   AttachmentSkipped,
			Name:   displayName,
			Reason: "no download URL",
		}, false, msg, nil
	}

	if rf.Size > opts.MaxDownloadBytes {
		reason := fmt.Sprintf("exceeds %s limit", formatLimitBytes(opts.MaxDownloadBytes))
		msg := fmt.Sprintf("skipped %s (%d bytes): %s", displayName, rf.Size, reason)
		return Attachment{
			Kind:     AttachmentSkipped,
			Name:     displayName,
			MIMEType: mimeType,
			Size:     rf.Size,
			Reason:   reason,
		}, false, msg, nil
	}

	if opts.Client == nil {
		msg := fmt.Sprintf("skipped %s: no API client configured", displayName)
		return Attachment{
			Kind:   AttachmentSkipped,
			Name:   displayName,
			Reason: "offline mode",
		}, false, msg, nil
	}

	maxInline := int64(-1)
	if isTextCandidate(rf, mimeType) && rf.Size <= opts.MaxInlineBytes {
		maxInline = opts.MaxInlineBytes
	}

	destPath := buildDestPath(attachmentDir, seq, rf.ID, displayName)
	inlineBytes, written, didSave, err := opts.Client.FetchPrivateFile(ctx, downloadURL, maxInline, opts.MaxDownloadBytes, destPath)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Attachment{}, false, "", err
		}
		reason := "download error"
		if errors.Is(err, slackapi.ErrFileTooLarge) {
			reason = fmt.Sprintf("exceeds %s limit", formatLimitBytes(opts.MaxDownloadBytes))
		} else if errors.Is(err, slackapi.ErrUnexpectedHTMLResponse) {
			reason = "authentication error (HTML response)"
		}
		msg := fmt.Sprintf("skipped %s: %v", displayName, err)
		return Attachment{
			Kind:     AttachmentSkipped,
			Name:     displayName,
			MIMEType: mimeType,
			Size:     rf.Size,
			Reason:   reason,
		}, false, msg, nil
	}

	if !didSave {
		return Attachment{
			Kind:     AttachmentInline,
			Name:     displayName,
			MIMEType: mimeType,
			Size:     written,
			Language: detectLanguageHint(rf, displayName),
			Content:  string(inlineBytes),
		}, false, "", nil
	}

	return Attachment{
		Kind:     AttachmentSaved,
		Name:     displayName,
		MIMEType: mimeType,
		Size:     written,
		Path:     destPath,
	}, true, "", nil
}

func isTextCandidate(rf slackapi.RawFile, mimeType string) bool {
	if rf.Mode == "snippet" {
		return true
	}
	if strings.HasPrefix(mimeType, "text/") {
		return true
	}
	switch mimeType {
	case "application/json",
		"application/xml",
		"application/yaml",
		"application/x-yaml",
		"application/javascript",
		"application/typescript",
		"application/sql":
		return true
	}
	return strings.HasSuffix(mimeType, "+json") || strings.HasSuffix(mimeType, "+xml")
}

func normalizeMIME(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(raw)
	if err == nil && mt != "" {
		return strings.ToLower(mt)
	}
	base, _, _ := strings.Cut(raw, ";")
	return strings.ToLower(strings.TrimSpace(base))
}

func detectLanguageHint(rf slackapi.RawFile, name string) string {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	candidate := ext
	if candidate == "" {
		candidate = strings.ToLower(strings.TrimSpace(rf.Filetype))
	}
	switch candidate {
	case "", "txt", "log", "text":
		return "text"
	case "yml":
		return "yaml"
	case "js":
		return "javascript"
	case "ts":
		return "typescript"
	case "md", "markdown":
		return "markdown"
	default:
		var clean strings.Builder
		for _, r := range candidate {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '+' {
				clean.WriteRune(r)
			}
		}
		if clean.Len() == 0 {
			return "text"
		}
		return clean.String()
	}
}

func buildDestPath(dir string, seq int, fileID, rawName string) string {
	safeID := sanitizeID(fileID)
	safeName := SanitizeFileName(rawName)
	fileName := fmt.Sprintf("%02d_%s_%s", seq, safeID, safeName)
	return filepath.Join(dir, fileName)
}

func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "FILE"
	}
	return b.String()
}

// SanitizeFileName neutralizes path traversal, control characters, and leading dots.
func SanitizeFileName(name string) string {
	normalized := strings.ReplaceAll(name, "\\", "/")
	base := path.Base(normalized)

	var b strings.Builder
	for _, r := range base {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == ':' {
			b.WriteByte('_')
			continue
		}
		if unicode.IsControl(r) {
			b.WriteByte('_')
			continue
		}
		b.WriteRune(r)
	}

	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "attachment"
	}
	if strings.HasPrefix(cleaned, ".") {
		cleaned = "_" + cleaned[1:]
	}

	const maxLen = 120
	if len(cleaned) > maxLen {
		ext := path.Ext(cleaned)
		if len(ext) > 0 && len(ext) < 20 && utf8.ValidString(ext) {
			stem := cleaned[:len(cleaned)-len(ext)]
			cleaned = truncateUTF8Bytes(stem, maxLen-len(ext)) + ext
		} else {
			cleaned = truncateUTF8Bytes(cleaned, maxLen)
		}
	}
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "attachment"
	}
	return cleaned
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := 0
	for i := range s {
		if i > maxBytes {
			break
		}
		cut = i
	}
	return s[:cut]
}

func formatLimitBytes(n int64) string {
	const mib = 1024 * 1024
	const kib = 1024
	if n%mib == 0 && n >= mib {
		return fmt.Sprintf("%d MiB", n/mib)
	}
	if n%kib == 0 && n >= kib {
		return fmt.Sprintf("%d KiB", n/kib)
	}
	return fmt.Sprintf("%d bytes", n)
}

// SafeMarkdownFence returns a backtick fence longer than any run in content.
func SafeMarkdownFence(content string) string {
	maxRun := 2
	current := 0
	for i := 0; i < len(content); i++ {
		if content[i] == '`' {
			current++
			if current > maxRun {
				maxRun = current
			}
		} else {
			current = 0
		}
	}
	return strings.Repeat("`", maxRun+1)
}
