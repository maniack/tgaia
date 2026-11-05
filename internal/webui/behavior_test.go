package webui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
)

func TestConversationBehaviorAPIIsScopedByBotAndChat(t *testing.T) {
	server, db := skillsServer(t)
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	if err := db.Agent().Accept(context.Background(), agent.Incoming{Scope: scope, UpdateID: 1, MessageID: 1, UserID: 7, ChatType: "supergroup", Title: "Team", Text: "hello", SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	server.chatBehavior(w, httptest.NewRequest("GET", "/api/chat-behavior?agent_id=test&bot_id=1&chat_id=100", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"group_response":"mentions"`) || !strings.Contains(w.Body.String(), `"loop_protection":true`) {
		t.Fatalf("default behavior response %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	server.putChatBehavior(w, httptest.NewRequest("POST", "/api/chat-behavior", strings.NewReader(`{"agent_id":"test","bot_id":1,"chat_id":100,"group_response":"all","respond_to_bots":true,"loop_protection":false,"version":1}`)))
	if w.Code != 200 {
		t.Fatalf("behavior update response %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	server.chatBehavior(w, httptest.NewRequest("GET", "/api/chat-behavior?agent_id=test&bot_id=1&chat_id=100", nil))
	for _, want := range []string{`"group_response":"all"`, `"respond_to_bots":true`, `"loop_protection":false`, `"version":2`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("updated behavior lacks %s: %s", want, w.Body.String())
		}
	}
}
