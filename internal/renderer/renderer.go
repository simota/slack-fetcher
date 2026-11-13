package renderer

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/shingoimota/slack-fetcher/internal/normalizer"
)

// RenderMarkdown formats a normalized Slack thread into YAML-frontmatted Markdown.
func RenderMarkdown(w io.Writer, doc normalizer.ThreadDocument) error {
	var b strings.Builder

	b.WriteString("---\n")
	fmt.Fprintf(&b, "source_url: %s\n", strconv.Quote(doc.SourceURL))
	fmt.Fprintf(&b, "channel_id: %s\n", strconv.Quote(doc.ChannelID))
	fmt.Fprintf(&b, "thread_ts: %s\n", strconv.Quote(doc.ThreadTS))
	if doc.FocusMessageTS != "" {
		fmt.Fprintf(&b, "focus_message_ts: %s\n", strconv.Quote(doc.FocusMessageTS))
	}
	fmt.Fprintf(&b, "fetched_at: %s\n", strconv.Quote(doc.FetchedAt.Format("2006-01-02T15:04:05Z07:00")))
	fmt.Fprintf(&b, "message_count: %d\n", len(doc.Messages))
	if len(doc.Participants) > 0 {
		b.WriteString("participants:\n")
		for _, p := range doc.Participants {
			fmt.Fprintf(&b, "  - %s\n", strconv.Quote(p))
		}
	} else {
		b.WriteString("participants: []\n")
	}
	if doc.AttachmentDir != "" {
		fmt.Fprintf(&b, "attachment_dir: %s\n", strconv.Quote(doc.AttachmentDir))
	}
	b.WriteString("---\n\n")

	fmt.Fprintf(&b, "# Slack Thread (%s / %s)\n", doc.ChannelID, doc.ThreadTS)

	for _, msg := range doc.Messages {
		b.WriteString("\n")
		tsLabel := msg.TS
		if !msg.Timestamp.IsZero() {
			tsLabel = msg.Timestamp.Format("2006-01-02 15:04:05Z07:00")
		}
		focusMarker := ""
		if msg.IsFocused {
			focusMarker = " [FOCUSED]"
		}
		fmt.Fprintf(&b, "## [%d] %s | %s (ts: %s)%s\n", msg.Index, tsLabel, msg.AuthorLabel, msg.TS, focusMarker)

		if strings.TrimSpace(msg.Body) != "" {
			b.WriteString("\n")
			b.WriteString(msg.Body)
			b.WriteString("\n")
		}

		if len(msg.Attachments) > 0 {
			b.WriteString("\n### Attachments\n")
			for _, att := range msg.Attachments {
				renderAttachment(&b, att)
			}
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func renderAttachment(b *strings.Builder, att normalizer.Attachment) {
	switch att.Kind {
	case normalizer.AttachmentInline:
		fmt.Fprintf(b, "- **Inline**: %s (%s, %s bytes)\n", inlineCodeSpan(att.Name), inlineCodeSpan(att.MIMEType), formatIntWithCommas(att.Size))
		fence := normalizer.SafeMarkdownFence(att.Content)
		lang := att.Language
		if lang == "" {
			lang = "text"
		}
		fmt.Fprintf(b, "%s%s\n", fence, lang)
		b.WriteString(att.Content)
		if !strings.HasSuffix(att.Content, "\n") {
			b.WriteByte('\n')
		}
		fmt.Fprintf(b, "%s\n", fence)

	case normalizer.AttachmentSaved:
		fmt.Fprintf(b, "- **Saved File**: %s (%s, %s bytes)\n", inlineCodeSpan(att.Name), inlineCodeSpan(att.MIMEType), formatIntWithCommas(att.Size))
		fmt.Fprintf(b, "  - Path: %s\n", inlineCodeSpan(att.Path))

	case normalizer.AttachmentUnavailable:
		fmt.Fprintf(b, "- **Unavailable**: (%s)\n", sanitizeSingleLine(att.Reason))

	case normalizer.AttachmentExternal:
		if att.Permalink != "" {
			label, safeURL := escapeMarkdownLink(att.Name, att.Permalink)
			fmt.Fprintf(b, "- **External Link**: [%s](%s)\n", label, safeURL)
		} else {
			fmt.Fprintf(b, "- **External Link**: %s\n", inlineCodeSpan(att.Name))
		}

	case normalizer.AttachmentSkipped:
		fmt.Fprintf(b, "- **Skipped**: %s (reason: %s)\n", inlineCodeSpan(att.Name), sanitizeSingleLine(att.Reason))
	}
}

func inlineCodeSpan(s string) string {
	cleaned := sanitizeSingleLine(s)
	maxRun := 0
	cur := 0
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] == '`' {
			cur++
			if cur > maxRun {
				maxRun = cur
			}
		} else {
			cur = 0
		}
	}
	fence := strings.Repeat("`", maxRun+1)
	if strings.HasPrefix(cleaned, "`") || strings.HasSuffix(cleaned, "`") {
		return fence + " " + cleaned + " " + fence
	}
	return fence + cleaned + fence
}

func escapeMarkdownLink(label, rawURL string) (string, string) {
	cleanLabel := sanitizeSingleLine(label)
	replacer := strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
	escapedLabel := replacer.Replace(cleanLabel)

	var urlBuilder strings.Builder
	for _, r := range rawURL {
		if r <= 0x20 || r == 0x7f || unicode.IsControl(r) {
			if r == ' ' {
				urlBuilder.WriteString("%20")
			}
			continue
		}
		switch r {
		case '(':
			urlBuilder.WriteString("%28")
		case ')':
			urlBuilder.WriteString("%29")
		case '<':
			urlBuilder.WriteString("%3C")
		case '>':
			urlBuilder.WriteString("%3E")
		default:
			urlBuilder.WriteRune(r)
		}
	}
	return escapedLabel, urlBuilder.String()
}

func sanitizeSingleLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\r' || r == '\n' || r == '\t' || r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func formatIntWithCommas(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
	}
	for i := rem; i < len(s); i += 3 {
		if len(out) > 0 {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}
