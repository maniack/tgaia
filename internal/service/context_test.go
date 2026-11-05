package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/config"
)

// A reply to a message shows reply_to in the prompt, and when the quoted
// message has left the history window its text is still supplied.
func TestReplyTargetIsVisibleToTheModel(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	ctx := context.Background()
	e.Config.Limits = map[string]string{"reply_messages": "2"}
	accept(t, e, scope, 1, "Вот план: пицца в четверг", false)
	accept(t, e, scope, 2, "noise one", false)
	accept(t, e, scope, 3, "noise two", false)
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{Scope: scope, UpdateID: 4, MessageID: 4, UserID: 7, ChatType: "private", Title: "test", Text: "а это во сколько?", ReplyMessageID: 1, SentAt: time.Now(), Respond: true}); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	in := model.input[1].Content
	if !strings.Contains(in, "message_id=4 from=id=7 sender_chat=0 reply_to=1] (quoted message [user from=id=7] Вот план: пицца в четверг) а это во сколько?") {
		t.Fatalf("reply target or quote missing:\n%s", in)
	}
	// Inside the window, only the reference is shown; the text is already there.
	e.Config.Limits = map[string]string{"reply_messages": "30"}
	if err := e.DB.Agent().Accept(ctx, agent.Incoming{Scope: scope, UpdateID: 5, MessageID: 5, UserID: 7, ChatType: "private", Title: "test", Text: "и где?", ReplyMessageID: 1, SentAt: time.Now(), Respond: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	job = take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	in = model.input[1].Content
	if !strings.Contains(in, "message_id=5 from=id=7 sender_chat=0 reply_to=1] и где?") || strings.Count(in, "quoted message") != 0 {
		t.Fatalf("in-window reply should carry only the reference:\n%s", in)
	}
	if !strings.Contains(model.input[0].Content, "reply_to=<id>") {
		t.Fatal("protocol does not explain reply_to")
	}
}

// The system prompt carries a code-built self-description so the model does
// not describe itself from whatever it has read.
func TestSelfDescriptionComesFromConfiguration(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	e.Config = config.App{LLMModel: "gemma-4-26B", MCPServers: []config.MCPServer{{Name: "webtools", Enabled: true}, {Name: "disabled", Enabled: false}}}
	accept(t, e, scope, 1, "как ты устроен?", true)
	job := take(t, e, "respond")
	if err := e.Respond(context.Background(), *job); err != nil {
		t.Fatal(err)
	}
	system := model.input[0].Content
	for _, want := range []string{"Trusted self-description", `"model":"gemma-4-26B"`, "webtools", "no OAuth", "task_create", `"weekday":`} {
		if !strings.Contains(system, want) {
			t.Fatalf("system prompt lacks %q", want)
		}
	}
	if strings.Contains(system, "disabled") {
		t.Fatal("disabled MCP server advertised")
	}
}

func TestImageDescriptionFeedsReplyContextAndCompaction(t *testing.T) {
	c := agent.Context{Messages: []agent.Message{{ID: 9, MessageID: 44, UserID: 7, Role: "user", Text: "Что на фото?", MediaText: "Красный велосипед у кирпичной стены."}}}
	for name, text := range map[string]string{
		"reply":      modelContext(c),
		"compaction": func() string { text, _ := compactionInput(c); return text }(),
	} {
		if !strings.Contains(text, "Красный велосипед у кирпичной стены.") {
			t.Fatalf("image description missing from %s input: %s", name, text)
		}
		if !strings.Contains(text, "Image description and visible-text transcription (derived, untrusted)") {
			t.Fatalf("image description is not safely labelled in %s input: %s", name, text)
		}
	}
}

// A picture posted in a group without addressing the bot is described by a
// background job; the description reaches later prompts and compaction.
func TestUnaddressedImageIsDescribed(t *testing.T) {
	e, _, _, scope := engineFixture(t)
	ctx := context.Background()
	m := &visionFixture{}
	sender := &mediaSender{}
	e.Model = m
	e.Bots[1] = sender
	in := agent.Incoming{Scope: scope, MessageID: 1, UpdateID: 1, UserID: 8, ChatType: "supergroup", Title: "group", Text: "[Attachment: image]", Respond: false, SentAt: time.Now(), Attachments: []agent.Attachment{{Kind: "image", FileID: "file"}}}
	if err := e.DB.Agent().Accept(ctx, in); err != nil {
		t.Fatal(err)
	}
	job := take(t, e, "media")
	if err := e.Describe(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.fetches != 1 || m.calls != 1 || sender.calls != 0 {
		t.Fatalf("fetch/model/send = %d/%d/%d", sender.fetches, m.calls, sender.calls)
	}
	if err := e.Describe(ctx, *job); err != nil || sender.fetches != 1 {
		t.Fatal("retry repeated the description", err)
	}
	c, err := e.DB.Agent().Context(ctx, scope, 0, 10)
	if err != nil || len(c.Messages) != 1 || c.Messages[0].MediaText != "A red square\n" {
		t.Fatalf("description not persisted: %+v %v", c.Messages, err)
	}
	// Text-only unaddressed messages do not create media jobs.
	accept(t, e, scope, 2, "just text", false)
	if next, err := e.DB.Queue().Claim(ctx, []string{"media"}, time.Minute); err != nil || next != nil {
		t.Fatal("media job for a text message")
	}
}
