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

type typingTransport func(*http.Request) (*http.Response, error)

func (f typingTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTypingRequest(t *testing.T) {
	c := New(config.NewSecret("fixture-token"))
	c.http.Transport = typingTransport(func(r *http.Request) (*http.Response, error) {
		var body struct {
			ChatID int64  `json:"chat_id"`
			Action string `json:"action"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ChatID != -123 || body.Action != "typing" || r.URL.Path != "/botfixture-token/sendChatAction" || r.Method != "POST" {
			t.Fatal("invalid typing request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)), Header: make(http.Header)}, nil
	})
	if err := c.Typing(context.Background(), -123); err != nil {
		t.Fatal(err)
	}
}
