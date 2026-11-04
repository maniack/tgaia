package telegram

import "testing"

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
