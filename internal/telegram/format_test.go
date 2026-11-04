package telegram

import "testing"

func TestMarkdownToTelegramHTML(t *testing.T) {
	source := "# Result\n\n- **bold** and `code`\n1. *italic* and ~~old~~\n> quoted <value>\n[site](https://example.invalid/a?x=1&y=2)\n<script>alert(1)</script>"
	want := "<b>Result</b>\n\n• <b>bold</b> and <code>code</code>\n1. <i>italic</i> and <s>old</s>\n<blockquote>quoted &lt;value&gt;</blockquote>\n<a href=\"https://example.invalid/a?x=1&amp;y=2\">site</a>\n&lt;script&gt;alert(1)&lt;/script&gt;"
	if got := markdownToTelegramHTML(source); got != want {
		t.Fatalf("unexpected Telegram HTML:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownToTelegramHTMLClosesCodeAndRejectsUnsafeLinks(t *testing.T) {
	source := "```go\nif a < b {\n\n[x](javascript:alert(1)) user_name"
	want := "<pre><code>if a &lt; b {\n\n[x](javascript:alert(1)) user_name</code></pre>"
	if got := markdownToTelegramHTML(source); got != want {
		t.Fatalf("unexpected Telegram HTML: %q, want %q", got, want)
	}
}
