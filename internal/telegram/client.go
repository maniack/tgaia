package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
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
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name"`
	LanguageCode string `json:"language_code"`
	IsPremium    bool   `json:"is_premium"`
}
type ChatMember struct {
	Status             string `json:"status"`
	CanDeleteMessages  bool   `json:"can_delete_messages"`
	CanRestrictMembers bool   `json:"can_restrict_members"`
	CanPinMessages     bool   `json:"can_pin_messages"`
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
	ThreadID       int64       `json:"message_thread_id"`
	IsTopicMessage bool        `json:"is_topic_message"`
	SenderChat     *SenderChat `json:"sender_chat"`
	EditDate       int64       `json:"edit_date"`
	Photo          []File      `json:"photo"`
	Document       *File       `json:"document"`
	Voice          *File       `json:"voice"`
	Audio          *File       `json:"audio"`
	Video          *File       `json:"video"`
	ID             int64       `json:"message_id"`
	Date           int64       `json:"date"`
	From           *User       `json:"from"`
	Chat           struct {
		ID      int64  `json:"id"`
		Type    string `json:"type"`
		Title   string `json:"title"`
		IsForum bool   `json:"is_forum"`
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
	threadID := int64(0)
	if m.Chat.IsForum && m.IsTopicMessage {
		threadID = m.ThreadID
	}
	input := agent.Incoming{Scope: chat.Scope{AgentID: agentID, BotID: botID, ChatID: m.Chat.ID, ThreadID: threadID}, UpdateID: update.ID, MessageID: m.ID, Text: text, ChatType: m.Chat.Type, Title: m.Chat.Title, IsForum: m.Chat.IsForum, SentAt: time.Unix(m.Date, 0).UTC()}
	if m.From != nil && m.SenderChat == nil {
		input.UserID = m.From.ID
		input.Username = m.From.Username
		input.Name = strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
		input.LanguageCode = m.From.LanguageCode
		input.IsPremium = m.From.IsPremium
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
	return c.callRaw(ctx, method, "application/json", data, dest)
}
func (c *Client) callRaw(ctx context.Context, method, contentType string, data []byte, dest any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+c.token.Reveal()+"/"+method, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Telegram endpoint")
	}
	req.Header.Set("Content-Type", contentType)
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
	if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 429 {
		// A definite refusal: nothing was sent, so a caller may safely try another
		// delivery form. The description is not surfaced; it can echo input.
		return rejectedError{fmt.Errorf("Telegram API rejected the request (HTTP %d)", response.StatusCode)}
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
	body := map[string]any{"chat_id": chatID, "text": markdownToTelegramHTML(text), "parse_mode": "HTML"}
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

// rejectedError marks a 4xx Bot API answer; see service.Rejected.
type rejectedError struct{ error }

func (rejectedError) Rejected() bool  { return true }
func (e rejectedError) Unwrap() error { return e.error }

// SendReply posts a short message quoting the user's original message. It is
// used for failure notices, so the user sees which request could not be served.
func (c *Client) SendReply(ctx context.Context, chatID, threadID, replyTo int64, text string) (int64, time.Time, error) {
	var m Message
	body := map[string]any{"chat_id": chatID, "text": markdownToTelegramHTML(text), "parse_mode": "HTML"}
	if threadID > 0 {
		body["message_thread_id"] = threadID
	}
	if replyTo > 0 {
		body["reply_parameters"] = map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}
	}
	if err := c.call(ctx, "sendMessage", body, &m); err != nil {
		return 0, time.Time{}, err
	}
	return m.ID, time.Unix(m.Date, 0).UTC(), nil
}

// SendDocument uploads one file through multipart/form-data. The caption is
// Markdown rendered to Telegram HTML like ordinary messages.
func (c *Client) SendDocument(ctx context.Context, chatID, threadID int64, name string, data []byte, caption string) (int64, time.Time, error) {
	if name == "" || len(name) > 128 || strings.ContainsAny(name, `/\`+"\x00") || len(data) == 0 || len(data) > 50<<20 {
		return 0, time.Time{}, errors.New("invalid document")
	}
	var buffer bytes.Buffer
	form := multipart.NewWriter(&buffer)
	_ = form.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if threadID > 0 {
		_ = form.WriteField("message_thread_id", strconv.FormatInt(threadID, 10))
	}
	if strings.TrimSpace(caption) != "" {
		_ = form.WriteField("caption", markdownToTelegramHTML(caption))
		_ = form.WriteField("parse_mode", "HTML")
	}
	part, err := form.CreateFormFile("document", name)
	if err != nil {
		return 0, time.Time{}, errors.New("invalid document upload")
	}
	if _, err := part.Write(data); err != nil {
		return 0, time.Time{}, errors.New("invalid document upload")
	}
	if err := form.Close(); err != nil {
		return 0, time.Time{}, errors.New("invalid document upload")
	}
	var m Message
	if err := c.callRaw(ctx, "sendDocument", form.FormDataContentType(), buffer.Bytes(), &m); err != nil {
		return 0, time.Time{}, err
	}
	if m.ID <= 0 {
		return 0, time.Time{}, errors.New("Telegram returned no message ID")
	}
	return m.ID, time.Unix(m.Date, 0).UTC(), nil
}

func (c *Client) Edit(ctx context.Context, chatID, messageID int64, text string) error {
	return c.call(ctx, "editMessageText", map[string]any{"chat_id": chatID, "message_id": messageID, "text": markdownToTelegramHTML(text), "parse_mode": "HTML"}, nil)
}
func (c *Client) Delete(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}

// ModerationPermissions asks Telegram for the bot's current membership and
// administrator rights. Configuration alone never grants a moderation tool.
func (c *Client) ModerationPermissions(ctx context.Context, chatID, botID int64) (map[string]bool, error) {
	var member ChatMember
	if err := c.call(ctx, "getChatMember", map[string]any{"chat_id": chatID, "user_id": botID}, &member); err != nil {
		return nil, err
	}
	admin := member.Status == "administrator" || member.Status == "creator"
	return map[string]bool{
		"delete":   admin && member.CanDeleteMessages,
		"restrict": admin && member.CanRestrictMembers,
		"ban":      admin && member.CanRestrictMembers,
		"pin":      admin && member.CanPinMessages,
	}, nil
}
func (c *Client) BanMember(ctx context.Context, chatID, userID int64) error {
	return c.call(ctx, "banChatMember", map[string]any{"chat_id": chatID, "user_id": userID}, nil)
}
func (c *Client) UnbanMember(ctx context.Context, chatID, userID int64) error {
	return c.call(ctx, "unbanChatMember", map[string]any{"chat_id": chatID, "user_id": userID, "only_if_banned": true}, nil)
}
func (c *Client) RestrictMember(ctx context.Context, chatID, userID, until int64) error {
	permissions := map[string]bool{"can_send_messages": false, "can_send_audios": false, "can_send_documents": false, "can_send_photos": false, "can_send_videos": false, "can_send_video_notes": false, "can_send_voice_notes": false, "can_send_polls": false, "can_send_other_messages": false, "can_add_web_page_previews": false, "can_change_info": false, "can_invite_users": false, "can_pin_messages": false, "can_manage_topics": false}
	return c.call(ctx, "restrictChatMember", map[string]any{"chat_id": chatID, "user_id": userID, "until_date": until, "permissions": permissions, "use_independent_chat_permissions": true}, nil)
}
func (c *Client) UnrestrictMember(ctx context.Context, chatID, userID int64) error {
	permissions := map[string]bool{"can_send_messages": true, "can_send_audios": true, "can_send_documents": true, "can_send_photos": true, "can_send_videos": true, "can_send_video_notes": true, "can_send_voice_notes": true, "can_send_polls": true, "can_send_other_messages": true, "can_add_web_page_previews": true, "can_change_info": true, "can_invite_users": true, "can_pin_messages": true, "can_manage_topics": true}
	return c.call(ctx, "restrictChatMember", map[string]any{"chat_id": chatID, "user_id": userID, "permissions": permissions, "use_independent_chat_permissions": true}, nil)
}
func (c *Client) PinMessage(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "pinChatMessage", map[string]any{"chat_id": chatID, "message_id": messageID, "disable_notification": true}, nil)
}
func (c *Client) UnpinMessage(ctx context.Context, chatID, messageID int64) error {
	return c.call(ctx, "unpinChatMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
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
