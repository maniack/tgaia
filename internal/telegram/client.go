package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
)

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}
type SenderChat struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}
type File struct {
	FileID string `json:"file_id"`
	Size   int64  `json:"file_size"`
	MIME   string `json:"mime_type"`
	Name   string `json:"file_name"`
}
type Message struct {
	ThreadID   int64       `json:"message_thread_id"`
	SenderChat *SenderChat `json:"sender_chat"`
	EditDate   int64       `json:"edit_date"`
	Photo      []File      `json:"photo"`
	Document   *File       `json:"document"`
	Voice      *File       `json:"voice"`
	Audio      *File       `json:"audio"`
	Video      *File       `json:"video"`
	ID         int64       `json:"message_id"`
	Date       int64       `json:"date"`
	From       *User       `json:"from"`
	Chat       struct {
		ID    int64  `json:"id"`
		Type  string `json:"type"`
		Title string `json:"title"`
	} `json:"chat"`
	Text            string   `json:"text"`
	Caption         string   `json:"caption"`
	Entities        []Entity `json:"entities"`
	CaptionEntities []Entity `json:"caption_entities"`
	Reply           *Message `json:"reply_to_message"`
}
type Update struct {
	EditedMessage     *Message `json:"edited_message"`
	EditedChannelPost *Message `json:"edited_channel_post"`
	ID                int64    `json:"update_id"`
	Message           *Message `json:"message"`
	ChannelPost       *Message `json:"channel_post"`
}

func Incoming(update Update, agentID string, botID int64, username string) (agent.Incoming, bool) {
	m := update.Message
	edited := false
	if m == nil && update.EditedMessage != nil {
		m = update.EditedMessage
		edited = true
	}
	if m == nil && update.EditedChannelPost != nil {
		m = update.EditedChannelPost
		edited = true
	}
	if m == nil {
		m = update.ChannelPost
	}
	if m == nil {
		return agent.Incoming{}, false
	}
	text := m.Text
	entities := m.Entities
	if text == "" {
		text = m.Caption
		entities = m.CaptionEntities
	}
	input := agent.Incoming{Scope: chat.Scope{AgentID: agentID, BotID: botID, ChatID: m.Chat.ID, ThreadID: m.ThreadID}, UpdateID: update.ID, MessageID: m.ID, Text: text, ChatType: m.Chat.Type, Title: m.Chat.Title, SentAt: time.Unix(m.Date, 0).UTC()}
	if m.From != nil && m.SenderChat == nil {
		input.UserID = m.From.ID
		input.Username = m.From.Username
		input.Name = m.From.FirstName
		if input.Title == "" {
			input.Title = m.From.FirstName
		}
	}
	if m.Reply != nil {
		input.ReplyMessageID = m.Reply.ID
	}
	input.Edited = edited
	if m.SenderChat != nil {
		input.SenderChatID = m.SenderChat.ID
		input.Name = m.SenderChat.Title
	}
	if m.Chat.Type == "channel" && input.UserID == 0 && input.SenderChatID == 0 {
		input.SenderChatID = m.Chat.ID
	}
	add := func(f File, kind string) {
		input.Attachments = append(input.Attachments, agent.Attachment{Kind: kind, FileID: f.FileID, Size: f.Size, MIME: f.MIME, Name: f.Name})
	}
	if len(m.Photo) > 0 {
		add(m.Photo[len(m.Photo)-1], "image")
	}
	if m.Document != nil {
		kind := "document"
		if strings.HasPrefix(m.Document.MIME, "image/") {
			kind = "image"
		}
		add(*m.Document, kind)
	}
	if m.Voice != nil {
		add(*m.Voice, "audio")
	}
	if m.Audio != nil {
		add(*m.Audio, "audio")
	}
	if m.Video != nil {
		add(*m.Video, "video")
	}
	if input.Text == "" && len(input.Attachments) > 0 {
		input.Text = "[Attachment: " + input.Attachments[0].Kind + "]"
	}
	input.Respond = m.Chat.Type == "private" || (m.Reply != nil && m.Reply.From != nil && m.Reply.From.ID == botID)
	units := utf16.Encode([]rune(text))
	for _, e := range entities {
		if e.Type != "mention" || e.Offset < 0 || e.Length < 1 || e.Offset > len(units) || e.Length > len(units)-e.Offset {
			continue
		}
		if strings.EqualFold(string(utf16.Decode(units[e.Offset:e.Offset+e.Length])), "@"+username) {
			input.Respond = true
		}
	}
	return input, true
}

type Client struct {
	limits config.App
	token  config.Secret
	http   *http.Client
}

func New(token config.Secret) *Client { return NewWithLimits(token, config.App{}) }
func NewWithLimits(token config.Secret, cfg config.App) *Client {
	return &Client{limits: cfg, token: token, http: &http.Client{Timeout: cfg.Duration("telegram_timeout"), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) call(ctx context.Context, method string, body any, dest any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+c.token.Reveal()+"/"+method, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Telegram endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return errors.New("Telegram request failed; outcome may be unknown")
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid Telegram response")
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return errors.New("invalid Telegram response")
	}
	// These two responses confirm an idempotent retry's intended state.
	if response.StatusCode == 400 && ((method == "editMessageText" && strings.HasPrefix(envelope.Description, "Bad Request: message is not modified")) || (method == "deleteMessage" && envelope.Description == "Bad Request: message to delete not found")) {
		return nil
	}
	if response.StatusCode != 200 || !envelope.OK {
		return fmt.Errorf("Telegram API did not confirm success (HTTP %d)", response.StatusCode)
	}
	if dest != nil && json.Unmarshal(envelope.Result, dest) != nil {
		return errors.New("invalid Telegram result")
	}
	return nil
}
func (c *Client) Identity(ctx context.Context) (User, error) {
	var u User
	err := c.call(ctx, "getMe", map[string]any{}, &u)
	return u, err
}
func (c *Client) Send(ctx context.Context, chatID int64, text string) (int64, time.Time, error) {
	return c.SendTopic(ctx, chatID, 0, text)
}
func (c *Client) SendTopic(ctx context.Context, chatID, threadID int64, text string) (int64, time.Time, error) {
	var m Message
	body := map[string]any{"chat_id": chatID, "text": text}
	if threadID > 0 {
		body["message_thread_id"] = threadID
	}
	err := c.call(ctx, "sendMessage", body, &m)
	if err != nil {
		return 0, time.Time{}, err
	}
	if m.ID <= 0 {
		return 0, time.Time{}, errors.New("Telegram returned no message ID")
	}
	return m.ID, time.Unix(m.Date, 0).UTC(), nil
}
func (c *Client) Edit(ctx context.Context, chatID, messageID int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}, nil)
}
func (c *Client) Delete(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}
func BotPath(id int64) string { return "/telegram/" + strconv.FormatInt(id, 10) }

func (c *Client) Updates(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "limit": 100, "allowed_updates": []string{"message", "channel_post", "edited_message", "edited_channel_post"}}, &updates)
	return updates, err
}

// Typing refreshes Telegram's transient typing indicator for this chat.
func (c *Client) Typing(ctx context.Context, chatID int64) error {
	return c.TypingTopic(ctx, chatID, 0)
}
func (c *Client) TypingTopic(ctx context.Context, chatID, threadID int64) error {
	body := map[string]any{"chat_id": chatID, "action": "typing"}
	if threadID > 0 {
		body["message_thread_id"] = threadID
	}
	return c.call(ctx, "sendChatAction", body, nil)
}
