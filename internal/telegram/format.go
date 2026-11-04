package telegram

import (
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// markdownToTelegramHTML converts the small, common Markdown subset emitted by
// chat models into Telegram's supported HTML subset. Raw HTML is always escaped.
func markdownToTelegramHTML(source string) string {
	lines := strings.Split(source, "\n")
	rendered := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "```") {
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			rendered = append(rendered, "<pre><code>"+html.EscapeString(strings.Join(code, "\n"))+"</code></pre>")
			continue
		}
		rendered = append(rendered, markdownBlockLine(lines[i]))
	}
	return strings.Join(rendered, "\n")
}

func markdownBlockLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if trimmed == "---" || trimmed == "***" || trimmed == "___" {
		return "—"
	}
	if strings.HasPrefix(trimmed, "> ") {
		return "<blockquote>" + markdownInline(strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))) + "</blockquote>"
	}
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "+ ") || strings.HasPrefix(trimmed, "* ") {
		return "• " + markdownInline(strings.TrimSpace(trimmed[2:]))
	}
	if n := orderedListPrefix(trimmed); n > 0 {
		return html.EscapeString(trimmed[:n]) + markdownInline(strings.TrimSpace(trimmed[n:]))
	}
	if n := headingPrefix(trimmed); n > 0 {
		return "<b>" + markdownInline(strings.TrimSpace(trimmed[n:])) + "</b>"
	}
	return markdownInline(line)
}

func headingPrefix(line string) int {
	i := 0
	for i < len(line) && i < 6 && line[i] == '#' {
		i++
	}
	if i > 0 && i < len(line) && line[i] == ' ' {
		return i + 1
	}
	return 0
}

func orderedListPrefix(line string) int {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && (line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
		return i + 2
	}
	return 0
}

func markdownInline(source string) string {
	var out strings.Builder
	for i := 0; i < len(source); {
		if source[i] == '\\' && i+1 < len(source) {
			_, size := utf8.DecodeRuneInString(source[i+1:])
			out.WriteString(html.EscapeString(source[i+1 : i+1+size]))
			i += 1 + size
			continue
		}
		if source[i] == '[' {
			if consumed, formatted := markdownLink(source[i:]); consumed > 0 {
				out.WriteString(formatted)
				i += consumed
				continue
			}
		}
		matched := false
		for _, delimiter := range []struct {
			mark, open, close string
			literal           bool
		}{{"**", "<b>", "</b>", false}, {"__", "<b>", "</b>", false}, {"~~", "<s>", "</s>", false}, {"||", "<tg-spoiler>", "</tg-spoiler>", false}, {"`", "<code>", "</code>", true}, {"*", "<i>", "</i>", false}, {"_", "<i>", "</i>", false}} {
			if !strings.HasPrefix(source[i:], delimiter.mark) {
				continue
			}
			start := i + len(delimiter.mark)
			relativeEnd := strings.Index(source[start:], delimiter.mark)
			if relativeEnd <= 0 {
				continue
			}
			end := start + relativeEnd
			if len(delimiter.mark) == 1 && !validEmphasis(source, i, start, end) {
				continue
			}
			content := source[start:end]
			out.WriteString(delimiter.open)
			if delimiter.literal {
				out.WriteString(html.EscapeString(content))
			} else {
				out.WriteString(markdownInline(content))
			}
			out.WriteString(delimiter.close)
			i = end + len(delimiter.mark)
			matched = true
			break
		}
		if matched {
			continue
		}
		_, size := utf8.DecodeRuneInString(source[i:])
		out.WriteString(html.EscapeString(source[i : i+size]))
		i += size
	}
	return out.String()
}

func validEmphasis(source string, opening, contentStart, closing int) bool {
	first, _ := utf8.DecodeRuneInString(source[contentStart:])
	last, _ := utf8.DecodeLastRuneInString(source[contentStart:closing])
	if unicode.IsSpace(first) || unicode.IsSpace(last) {
		return false
	}
	if source[opening] == '_' {
		if opening > 0 {
			previous, _ := utf8.DecodeLastRuneInString(source[:opening])
			if unicode.IsLetter(previous) || unicode.IsDigit(previous) {
				return false
			}
		}
		if closing+1 < len(source) {
			next, _ := utf8.DecodeRuneInString(source[closing+1:])
			if unicode.IsLetter(next) || unicode.IsDigit(next) {
				return false
			}
		}
	}
	return true
}

func markdownLink(source string) (int, string) {
	labelEnd := strings.Index(source, "](")
	if labelEnd <= 1 {
		return 0, ""
	}
	urlEnd := strings.IndexByte(source[labelEnd+2:], ')')
	if urlEnd < 1 {
		return 0, ""
	}
	urlEnd += labelEnd + 2
	rawURL := source[labelEnd+2 : urlEnd]
	parsed, err := url.Parse(rawURL)
	if err != nil || !allowedTelegramLink(parsed) {
		return 0, ""
	}
	formatted := `<a href="` + html.EscapeString(rawURL) + `">` + markdownInline(source[1:labelEnd]) + "</a>"
	return urlEnd + 1, formatted
}

func allowedTelegramLink(link *url.URL) bool {
	switch strings.ToLower(link.Scheme) {
	case "http", "https":
		return link.Host != ""
	case "tg", "mailto":
		return true
	default:
		return false
	}
}
