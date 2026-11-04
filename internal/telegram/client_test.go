package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/maniack/tgaia/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestSendAndEditUseTelegramHTML(t *testing.T) {
	var requests []map[string]any
	client := New(config.NewSecret("test-token"))
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		result := `{"ok":true,"result":{"message_id":7,"date":1}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(result)), Header: make(http.Header)}, nil
	})
	if _, _, err := client.Send(context.Background(), 1, "**bold** <tag>"); err != nil {
		t.Fatal(err)
	}
	if err := client.Edit(context.Background(), 1, 7, "`code`"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0]["parse_mode"] != "HTML" || requests[0]["text"] != "<b>bold</b> &lt;tag&gt;" {
		t.Fatalf("send did not use safe Telegram HTML: %#v", requests)
	}
	if requests[1]["parse_mode"] != "HTML" || requests[1]["text"] != "<code>code</code>" {
		t.Fatalf("edit did not use safe Telegram HTML: %#v", requests[1])
	}
}

func TestResponseTriggers(t *testing.T) {
	for _, tc := range []struct {
		name, kind, text string
		entities         []Entity
		reply            int64
		want             bool
	}{
		{name: "private", kind: "private", text: "hello", want: true},
		{name: "group passive", kind: "group", text: "hello"},
		{name: "utf16 mention", kind: "supergroup", text: "😀 @MyBot", entities: []Entity{{Type: "mention", Offset: 3, Length: 6}}, want: true},
		{name: "other bot", kind: "group", text: "@Other", entities: []Entity{{Type: "mention", Offset: 0, Length: 6}}},
		{name: "plain substring", kind: "group", text: "@MyBot"},
		{name: "reply own", kind: "group", text: "hello", reply: 42, want: true},
		{name: "reply other", kind: "group", text: "hello", reply: 43},
		{name: "invalid offset", kind: "group", text: "x", entities: []Entity{{Type: "mention", Offset: 99, Length: 100}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Message{ID: 1, Date: 1700000000, Text: tc.text, Entities: tc.entities, From: &User{ID: 7}}
			m.Chat.ID = 123
			m.Chat.Type = tc.kind
			if tc.reply != 0 {
				m.Reply = &Message{From: &User{ID: tc.reply}}
			}
			in, ok := Incoming(Update{ID: 1, Message: m}, "agent", 42, "mybot")
			if !ok || in.Respond != tc.want {
				t.Fatalf("respond=%v want=%v", in.Respond, tc.want)
			}
		})
	}
}
