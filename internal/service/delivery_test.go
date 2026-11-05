package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
)

func TestSplitMessageKeepsCodeFences(t *testing.T) {
	var lines []string
	for i := 0; i < 400; i++ {
		lines = append(lines, "console.log('line "+strings.Repeat("x", 20)+"');")
	}
	text := "Intro paragraph.\n\n```js\n" + strings.Join(lines, "\n") + "\n```\nOutro."
	chunks := splitMessage(text, messageLimit)
	if len(chunks) < 3 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > messageLimit {
			t.Fatalf("chunk %d exceeds limit", i)
		}
		if strings.Count(chunk, "```")%2 != 0 {
			t.Fatalf("chunk %d leaves a code fence open: %q", i, chunk[:60])
		}
		if i > 0 && i < len(chunks)-1 && !strings.HasPrefix(chunk, "```js\n") {
			t.Fatalf("chunk %d does not reopen the fence with its language", i)
		}
		if strings.Contains(chunk, "');con") {
			t.Fatalf("chunk %d cut inside a line", i)
		}
	}
	if strings.Join(chunks, "") == "" || !strings.HasSuffix(chunks[len(chunks)-1], "Outro.") {
		t.Fatal("tail lost")
	}
}

func TestPlanLongCodeReplyBecomesSourceDocument(t *testing.T) {
	code := strings.Repeat("<div>row</div>\n", 400)
	reply := "Вот игра 2048. Откройте файл в браузере.\n\n```html\n" + code + "```\n\nУдачи!"
	pieces := plan(reply, true)
	if len(pieces) != 1 || !pieces[0].document || pieces[0].file != "reply.html" {
		t.Fatalf("expected one html document, got %+v", pieces)
	}
	if string(pieces[0].data) != code || !strings.Contains(pieces[0].text, "Откройте файл") || strings.Contains(pieces[0].text, "```") {
		t.Fatal("document must contain only the code and caption only the prose")
	}
	mixed := strings.Repeat("Paragraph of prose.\n\n", 300)
	pieces = plan(mixed, true)
	if len(pieces) != 1 || pieces[0].file != "reply.md" || utf8.RuneCountInString(pieces[0].text) > captionLimit+1 {
		t.Fatalf("expected markdown document with bounded caption, got %+v", pieces[0].file)
	}
	if got := plan(mixed, false); len(got) < 2 || got[0].document {
		t.Fatal("senders without documents must fall back to multipart text")
	}
	if got := plan("short", true); len(got) != 1 || got[0].document {
		t.Fatal("short replies stay plain messages")
	}
}

type documentSender struct {
	testSender
	reject    bool
	documents []string
	captions  []string
}

func (s *documentSender) SendDocument(_ context.Context, _, _ int64, name string, data []byte, caption string) (int64, time.Time, error) {
	if s.reject {
		return 0, time.Time{}, rejectedErr{}
	}
	s.documents = append(s.documents, name+":"+string(data))
	s.captions = append(s.captions, caption)
	return int64(50000 + len(s.documents)), time.Now().UTC(), nil
}

type rejectedErr struct{}

func (rejectedErr) Error() string  { return "rejected" }
func (rejectedErr) Rejected() bool { return true }

func TestLongReplyDeliveredAsDocumentWithTextFallback(t *testing.T) {
	for _, reject := range []bool{false, true} {
		e, m, _, scope := engineFixture(t)
		sender := &documentSender{reject: reject}
		e.Bots[1] = sender
		code := strings.Repeat("let x = 1;\n", 500)
		raw, _ := json.Marshal(map[string]any{"reply": "Готово:\n\n```js\n" + code + "```"})
		m.reply = string(raw)
		accept(t, e, scope, 1, "write code", true)
		job := take(t, e, "respond")
		if err := e.Respond(context.Background(), *job); err != nil {
			t.Fatal(err)
		}
		if reject {
			if len(sender.documents) != 0 || sender.calls < 2 {
				t.Fatalf("rejected document must fall back to multipart text, got %d messages", sender.calls)
			}
			continue
		}
		if len(sender.documents) != 1 || !strings.HasPrefix(sender.documents[0], "reply.js:let x") || sender.captions[0] != "Готово:" || sender.calls != 0 {
			t.Fatalf("expected one source document with caption, got %+v / %d messages", sender.documents, sender.calls)
		}
		state, err := e.DB.Agent().DeliveryState(context.Background(), job.ID, 0)
		if err != nil || state != "sent" {
			t.Fatal("document delivery not recorded", state, err)
		}
	}
}

// singlePassFixture answers with plain text on the first tool turn and then
// serves the final closed-schema call.
type thinkingFixture struct {
	turns, finals int
	lastFinal     []llm.Message
	lastSchema    string
}

func (f *thinkingFixture) Open(context.Context, chat.Scope) (mcpclient.Session, error) {
	return f, nil
}
func (f *thinkingFixture) Tools() []llm.Tool {
	return []llm.Tool{{Type: "function", Function: llm.ToolFunction{Name: "lookup"}}}
}
func (f *thinkingFixture) Validate(string, string) error { return nil }
func (f *thinkingFixture) Call(context.Context, string, string) (string, error) {
	return "{}", nil
}
func (f *thinkingFixture) Close() {}
func (f *thinkingFixture) Complete(context.Context, []llm.Message) (string, error) {
	return "", errors.New("unexpected plain completion")
}
func (f *thinkingFixture) CompleteJSON(_ context.Context, in []llm.Message, schema json.RawMessage) (string, error) {
	f.finals++
	f.lastFinal = in
	f.lastSchema = string(schema)
	return `{"reply":"Напомню через 5 минут."}`, nil
}
func (f *thinkingFixture) CompleteTools(_ context.Context, in []llm.Message, _ []llm.Tool) (llm.Message, error) {
	f.turns++
	if in[len(in)-1].Role == "tool" {
		return llm.Message{Role: "assistant", ReasoningContent: "Thought: done.", Content: "Thought: the task is created. Internal note: answer briefly."}, nil
	}
	return llm.Message{Role: "assistant", ReasoningContent: "Thought: schedule it.", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.ToolFunction{Name: toolTaskCreate, Arguments: `{"prompt":"Напомни пользователю выпить воды сейчас","when":{"date":"","weekday":"","time":"","timezone":"","delay_seconds":300},"interval_seconds":0}`}}}}, nil
}

// A tool-phase text turn is an internal note. Only the closed-schema reply
// reaches the user; thoughts and notes never do.
func TestToolPhaseThoughtsNeverReachTheUser(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	f := &thinkingFixture{}
	e.Tools = f
	e.Model = f
	accept(t, e, scope, 1, "напомни через 5 минут выпить воды", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	if f.turns != 2 || f.finals != 1 || sender.calls != 1 {
		t.Fatalf("expected two tool turns, one structured final call and one message, got %d/%d/%d", f.turns, f.finals, sender.calls)
	}
	if !strings.Contains(f.lastSchema, `"reply"`) || !strings.Contains(f.lastFinal[len(f.lastFinal)-1].Content, "Tool phase is finished") {
		t.Fatal("final call must use the full response schema over the tool transcript")
	}
	result, err := e.DB.Agent().Result(context.Background(), job.ID)
	if err != nil || strings.Contains(result, "Thought") || strings.Contains(result, "Internal note") || !strings.Contains(result, "Напомню через 5 минут.") {
		t.Fatal("tool-phase thoughts leaked into the stored reply", result)
	}
	d, err := e.DB.Agent().Dashboard(context.Background())
	if err != nil || len(d.Tasks) != 1 {
		t.Fatal("task from the tool phase was not created")
	}
	if !strings.Contains(sender.last, "⏰ First run:") {
		t.Fatalf("code-generated schedule confirmation missing from the delivered text: %q", sender.last)
	}
}

func TestFailedNoticeRepliesToUser(t *testing.T) {
	e, _, sender, scope := engineFixture(t)
	accept(t, e, scope, 1, "long request", true)
	job := take(t, e, "respond")
	if err := e.DB.Queue().Fail(context.Background(), job.Receipt, "llm_timeout"); err != nil {
		t.Fatal(err)
	}
	e.Failed(context.Background(), *job, "llm_timeout")
	if sender.calls != 0 {
		t.Fatal("failure callback sent outside the durable queue")
	}
	notice := take(t, e, "failure_notice")
	if err := e.FailureNotice(context.Background(), *notice); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 1 {
		t.Fatalf("expected one failure notice, got %d", sender.calls)
	}
	if !strings.Contains(sender.last, "слишком много времени") {
		t.Fatalf("wrong classified notice: %q", sender.last)
	}
}

func TestFailureNoticeVocabularyIsContentFree(t *testing.T) {
	classes := []string{"llm_429", "llm_http_5xx", "llm_http_4xx", "llm_network", "llm_invalid", "embedding_timeout", "embedding_failed", "mcp_timeout", "mcp_failed", "quota", "budget", "job_timeout", "panic", "handler_error", "other", "unknown"}
	for _, class := range classes {
		if text := failureNoticeText(class, false); !strings.HasPrefix(text, "⚠️ ") || strings.Contains(text, class) {
			t.Fatalf("unsafe or malformed notice for %s: %q", class, text)
		}
	}
}
