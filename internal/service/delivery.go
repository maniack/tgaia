package service

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

// messageLimit is the per-message budget in runes, below Telegram's 4096 so
// HTML conversion and entity expansion still fit.
const messageLimit = 3500

// captionLimit keeps document captions under Telegram's 1024-character cap
// after formatting.
const captionLimit = 900

// documentLimit bounds the generated file size well below Telegram's 50 MB.
const documentLimit = 1 << 20

// DocumentSender delivers one file with an optional caption. Senders without
// this capability receive multipart text instead.
type DocumentSender interface {
	SendDocument(ctx context.Context, chatID, threadID int64, name string, data []byte, caption string) (int64, time.Time, error)
}

// Rejected marks a delivery error that Telegram definitely refused (HTTP 4xx):
// nothing was sent, so a different delivery form may be attempted safely.
type Rejected interface{ Rejected() bool }

type piece struct {
	text     string // Message text, or the caption when file is set.
	file     string // File name; empty for a plain text message.
	data     []byte
	document bool
}

// plan splits a reply into deliverable pieces. Long replies become a single
// document when the sender supports it: a lone fenced code block is delivered
// as runnable source with the surrounding prose as caption, anything else as
// Markdown. Without document support the text is split at paragraph or line
// boundaries without breaking code fences.
func plan(reply string, documents bool) []piece {
	if utf8.RuneCountInString(reply) <= messageLimit {
		return []piece{{text: reply}}
	}
	if documents && len(reply) <= documentLimit {
		if name, code, caption, ok := codeDocument(reply); ok {
			return []piece{{text: caption, file: name, data: []byte(code), document: true}}
		}
		return []piece{{text: captionFor(reply), file: "reply.md", data: []byte(reply), document: true}}
	}
	var pieces []piece
	for _, chunk := range splitMessage(reply, messageLimit) {
		pieces = append(pieces, piece{text: chunk})
	}
	return pieces
}

var codeExtensions = map[string]string{"html": "html", "htm": "html", "xhtml": "html", "js": "js", "javascript": "js", "mjs": "mjs", "ts": "ts", "typescript": "ts", "tsx": "tsx", "jsx": "jsx", "py": "py", "python": "py", "go": "go", "golang": "go", "rs": "rs", "rust": "rs", "java": "java", "kt": "kt", "kotlin": "kt", "swift": "swift", "c": "c", "h": "h", "cpp": "cpp", "c++": "cpp", "cc": "cpp", "hpp": "hpp", "cs": "cs", "csharp": "cs", "rb": "rb", "ruby": "rb", "php": "php", "sh": "sh", "bash": "sh", "zsh": "sh", "shell": "sh", "ps1": "ps1", "powershell": "ps1", "sql": "sql", "css": "css", "scss": "scss", "json": "json", "yaml": "yaml", "yml": "yaml", "toml": "toml", "xml": "xml", "md": "md", "markdown": "md", "txt": "txt", "text": "txt", "lua": "lua", "r": "r", "dart": "dart", "scala": "scala", "svg": "svg", "dockerfile": "Dockerfile", "makefile": "Makefile", "ini": "ini", "csv": "csv"}

// codeDocument recognizes a reply that is essentially one fenced code block
// with a language tag and short prose around it.
func codeDocument(reply string) (name, code, caption string, ok bool) {
	lines := strings.Split(reply, "\n")
	start, end := -1, -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if start < 0 {
				start = i
			} else if end < 0 {
				end = i
			} else {
				return "", "", "", false // More than one block.
			}
		}
	}
	if start < 0 || end < 0 {
		return "", "", "", false
	}
	lang := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[start]), "```")))
	if i := strings.IndexAny(lang, " \t{"); i >= 0 {
		lang = lang[:i]
	}
	ext := codeExtensions[lang]
	if ext == "" {
		return "", "", "", false
	}
	code = strings.Join(lines[start+1:end], "\n")
	if strings.TrimSpace(code) == "" {
		return "", "", "", false
	}
	prose := strings.TrimSpace(strings.Join(lines[:start], "\n") + "\n" + strings.Join(lines[end+1:], "\n"))
	if utf8.RuneCountInString(prose) > captionLimit {
		return "", "", "", false
	}
	switch ext {
	case "Dockerfile", "Makefile":
		name = ext
	default:
		name = "reply." + ext
	}
	if strings.HasSuffix(code, "\n") {
		return name, code, prose, true
	}
	return name, code + "\n", prose, true
}

// captionFor takes the opening prose of a reply, cut at a paragraph or line
// boundary, as the caption of a Markdown document.
func captionFor(reply string) string {
	head := reply
	if i := strings.Index(reply, "```"); i >= 0 {
		head = reply[:i]
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return "Full reply attached."
	}
	runes := []rune(head)
	if len(runes) <= captionLimit {
		return head
	}
	cut := string(runes[:captionLimit])
	if i := strings.LastIndex(cut, "\n\n"); i > captionLimit/3 {
		cut = cut[:i]
	} else if i := strings.LastIndex(cut, "\n"); i > captionLimit/3 {
		cut = cut[:i]
	} else if i := strings.LastIndex(cut, " "); i > captionLimit/3 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// splitMessage cuts text into chunks of at most limit runes, preferring blank
// lines, then line ends, then spaces. Open code fences are closed at the end
// of a chunk and reopened, with the same language tag, at the start of the next.
func splitMessage(text string, limit int) []string {
	var chunks []string
	fence := "" // Opening line of the currently open fence, or empty.
	rest := text
	for rest != "" {
		prefix := ""
		if fence != "" {
			prefix = fence + "\n"
		}
		budget := limit - utf8.RuneCountInString(prefix)
		if fence != "" {
			budget -= 4 // Room for the closing fence line.
		}
		runes := []rune(rest)
		if len(runes) <= budget {
			chunk := prefix + rest
			if fence != "" && !closesFence(rest, fence) {
				chunk += "\n```"
			}
			chunks = append(chunks, chunk)
			break
		}
		window := string(runes[:budget])
		cut := -1
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(window, sep); i > budget/4 {
				cut = i + len(sep)
				break
			}
		}
		if cut <= 0 {
			cut = len(window)
		}
		body := rest[:cut]
		rest = rest[cut:]
		fence = fenceState(fence, body)
		chunk := prefix + strings.TrimRight(body, " \n")
		if fence != "" {
			chunk += "\n```"
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}

// fenceState returns the open fence after processing body, given the fence
// that was open before it.
func fenceState(open, body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "```") {
			continue
		}
		if open == "" {
			open = trimmed
		} else {
			open = ""
		}
	}
	return open
}

func closesFence(body, open string) bool { return fenceState(open, body) == "" }
