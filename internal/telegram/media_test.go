package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/config"
)

func TestEditedTopicAndAnonymousSender(t *testing.T) {
	var update Update
	raw := `{"update_id":5,"edited_message":{"message_id":9,"message_thread_id":42,"is_topic_message":true,"date":1000,"edit_date":2000,"from":{"id":123456789},"sender_chat":{"id":-100,"title":"Anonymous group"},"chat":{"id":-100,"type":"supergroup","is_forum":true},"caption":"@bot inspect","caption_entities":[{"type":"mention","offset":0,"length":4}],"photo":[{"file_id":"small","file_size":10},{"file_id":"large","file_size":20}]}}`
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatal(err)
	}
	in, ok := Incoming(update, "a", 99, "bot")
	if !ok || !in.Edited || !in.Respond || in.UserID != 0 || in.SenderChatID != -100 || in.Scope.ThreadID != 42 || len(in.Attachments) != 1 || in.Attachments[0].FileID != "large" {
		t.Fatalf("wrong normalization: %+v", in)
	}
	var channel Update
	json.Unmarshal([]byte(`{"update_id":6,"channel_post":{"message_id":1,"date":1000,"chat":{"id":-200,"type":"channel"},"text":"passive"}}`), &channel)
	in, ok = Incoming(channel, "a", 99, "bot")
	if !ok || in.Respond || in.SenderChatID != -200 || in.UserID != 0 {
		t.Fatal("wrong channel policy")
	}
}
func TestMediaDownloadBoundary(t *testing.T) {
	for _, path := range []string{"documents/test.txt", "../escape", "https://elsewhere/file", "/absolute", "files/a?secret=x"} {
		t.Run(path, func(t *testing.T) {
			c := New(config.NewSecret("fixture-token"))
			downloads := 0
			c.http.Transport = typingTransport(func(r *http.Request) (*http.Response, error) {
				body := "hello text"
				if r.Method == "POST" {
					b, _ := json.Marshal(map[string]any{"ok": true, "result": map[string]any{"file_path": path, "file_size": 10}})
					body = string(b)
				} else {
					downloads++
					if r.URL.Host != "api.telegram.org" {
						t.Fatal("download origin changed")
					}
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			data, _, err := c.Fetch(context.Background(), agent.Attachment{Kind: "document", FileID: "file"})
			if path == "documents/test.txt" {
				if err != nil || string(data) != "hello text" || downloads != 1 {
					t.Fatal(err)
				}
			} else if err == nil || downloads != 0 {
				t.Fatal("unsafe path accepted")
			}
			if _, _, err := c.Fetch(context.Background(), agent.Attachment{Kind: "audio", FileID: "file"}); err == nil {
				t.Fatal("audio enabled")
			}
		})
	}
}
func TestTopicSendPayload(t *testing.T) {
	c := New(config.NewSecret("fixture-token"))
	c.http.Transport = typingTransport(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["message_thread_id"] != float64(42) || body["chat_id"] != float64(-100) {
			t.Fatal(body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":5,"date":1000}}`))}, nil
	})
	if _, _, err := c.SendTopic(context.Background(), -100, 42, "reply"); err != nil {
		t.Fatal(err)
	}
}
