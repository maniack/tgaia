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

func TestThreadIDRequiresForumTopic(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      int64
	}{
		{name: "ordinary reply thread", raw: `{"update_id":1,"message":{"message_id":7,"message_thread_id":99,"date":1,"from":{"id":5},"chat":{"id":10,"type":"supergroup"},"text":"hello"}}`},
		{name: "forum flag without topic message", raw: `{"update_id":2,"message":{"message_id":8,"message_thread_id":99,"date":1,"from":{"id":5},"chat":{"id":10,"type":"supergroup","is_forum":true},"text":"hello"}}`},
		{name: "forum topic", raw: `{"update_id":3,"message":{"message_id":9,"message_thread_id":99,"is_topic_message":true,"date":1,"from":{"id":5},"chat":{"id":10,"type":"supergroup","is_forum":true},"text":"hello"}}`, want: 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var update Update
			if err := json.Unmarshal([]byte(tc.raw), &update); err != nil {
				t.Fatal(err)
			}
			in, ok := Incoming(update, "agent", 42, "bot")
			if !ok || in.Scope.ThreadID != tc.want {
				t.Fatalf("thread=%d want=%d", in.Scope.ThreadID, tc.want)
			}
		})
	}
}

func TestIncomingCarriesTelegramProfile(t *testing.T) {
	m := &Message{ID: 1, Date: 1, Text: "hello", From: &User{ID: 7, Username: "ada", FirstName: "Ada", LastName: "Lovelace", LanguageCode: "en", IsPremium: true}}
	m.Chat.ID = 10
	m.Chat.Type = "private"
	in, ok := Incoming(Update{ID: 1, Message: m}, "agent", 42, "bot")
	if !ok || in.Name != "Ada Lovelace" || in.Username != "ada" || in.LanguageCode != "en" || !in.IsPremium {
		t.Fatalf("unexpected Telegram profile: %+v", in)
	}
}

func TestProfileCollectsTelegramIdentityAndLargestAvatar(t *testing.T) {
	client := New(config.NewSecret("test-token"))
	methods := []string{}
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		method := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		methods = append(methods, method)
		result := map[string]any{}
		switch method {
		case "getMe":
			result = map[string]any{"id": 42, "username": "walrus_bot", "first_name": "Cyber", "last_name": "Walrus"}
		case "getMyShortDescription":
			result = map[string]any{"short_description": "A concise walrus bio"}
		case "getMyDescription":
			result = map[string]any{"description": "A longer description of the walrus assistant"}
		case "getUserProfilePhotos":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["user_id"] != float64(42) || body["limit"] != float64(1) {
				t.Fatalf("unexpected profile photo request: %#v %v", body, err)
			}
			result = map[string]any{"total_count": 1, "photos": [][]map[string]any{{{"file_id": "small", "file_unique_id": "same", "width": 80, "height": 80, "file_size": 100}, {"file_id": "large", "file_unique_id": "same", "width": 640, "height": 640, "file_size": 200}}}}
		default:
			t.Fatalf("unexpected Telegram method %s", method)
		}
		data, _ := json.Marshal(map[string]any{"ok": true, "result": result})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(data))), Header: make(http.Header)}, nil
	})
	profile, err := client.Profile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != 42 || profile.FirstName != "Cyber" || profile.LastName != "Walrus" || profile.Username != "walrus_bot" || profile.Bio != "A concise walrus bio" || profile.Description != "A longer description of the walrus assistant" || profile.Avatar == nil || profile.Avatar.FileID != "large" || profile.Avatar.UniqueID != "same" {
		t.Fatalf("unexpected bot profile: %+v", profile)
	}
	want := []string{"getMe", "getMyShortDescription", "getMyDescription", "getUserProfilePhotos"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("methods=%v want=%v", methods, want)
	}
}

func TestModerationRequestsUseExactTelegramPayloads(t *testing.T) {
	type captured struct {
		method string
		body   map[string]any
	}
	var requests []captured
	client := New(config.NewSecret("test-token"))
	client.http.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		method := request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		requests = append(requests, captured{method: method, body: body})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})
	ctx := context.Background()
	for _, action := range []func() error{
		func() error { return client.BanMember(ctx, -100, 7) },
		func() error { return client.UnbanMember(ctx, -100, 7) },
		func() error { return client.RestrictMember(ctx, -100, 7, 12345) },
		func() error { return client.UnrestrictMember(ctx, -100, 7) },
		func() error { return client.PinMessage(ctx, -100, 99) },
		func() error { return client.UnpinMessage(ctx, -100, 99) },
	} {
		if err := action(); err != nil {
			t.Fatal(err)
		}
	}
	wantMethods := []string{"banChatMember", "unbanChatMember", "restrictChatMember", "restrictChatMember", "pinChatMessage", "unpinChatMessage"}
	if len(requests) != len(wantMethods) {
		t.Fatalf("requests=%d", len(requests))
	}
	for i, want := range wantMethods {
		if requests[i].method != want || requests[i].body["chat_id"] != float64(-100) {
			t.Fatalf("request %d: %+v", i, requests[i])
		}
	}
	if requests[1].body["only_if_banned"] != true || requests[2].body["until_date"] != float64(12345) || requests[4].body["disable_notification"] != true {
		t.Fatalf("moderation options missing: %+v", requests)
	}
	for _, i := range []int{2, 3} {
		permissions, ok := requests[i].body["permissions"].(map[string]any)
		if !ok || permissions["can_send_messages"] != (i == 3) || requests[i].body["use_independent_chat_permissions"] != true {
			t.Fatalf("restriction payload %d: %+v", i, requests[i].body)
		}
	}
}
