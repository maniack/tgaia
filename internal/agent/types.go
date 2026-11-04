package agent

import (
	"github.com/maniack/tgaia/internal/chat"
	"time"
)

type Attachment struct {
	Kind   string `json:"kind"`
	FileID string `json:"file_id"`
	MIME   string `json:"mime_type"`
	Name   string `json:"file_name"`
	Size   int64  `json:"file_size"`
}
type Incoming struct {
	ReplyMessageID int64
	SenderChatID   int64
	Edited         bool
	Attachments    []Attachment
	Scope          chat.Scope
	UpdateID       int64
	MessageID      int64
	UserID         int64
	Username       string
	Name           string
	ChatType       string
	Title          string
	Text           string
	SentAt         time.Time
	Respond        bool
}
type Message struct {
	ThreadID     int64  `json:"thread_id"`
	SenderChatID int64  `json:"sender_chat_id"`
	UpdateID     int64  `json:"update_id"`
	Attachments  string `json:"attachments"`
	MediaText    string `json:"media_text"`
	ID           int64  `json:"id"`
	MessageID    int64  `json:"message_id"`
	UserID       int64  `json:"user_id"`
	Role         string `json:"role"`
	Text         string `json:"text"`
	SentAt       int64  `json:"sent_at"`
}
type Task struct {
	ThreadID   int64  `json:"thread_id"`
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	BotID      int64  `json:"bot_id"`
	ChatID     int64  `json:"chat_id"`
	UserID     int64  `json:"user_id"`
	Prompt     string `json:"prompt"`
	RunAt      int64  `json:"run_at"`
	IntervalMS int64  `json:"interval_ms"`
	Active     bool   `json:"active"`
	Version    int64  `json:"version"`
}
type Conversation struct {
	ThreadID int64  `json:"thread_id"`
	AgentID  string `json:"agent_id"`
	BotID    int64  `json:"bot_id"`
	ChatID   int64  `json:"chat_id"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Blocked  bool   `json:"blocked"`
}
type Summary struct {
	Content   string `json:"content"`
	ThroughID int64  `json:"through_id"`
}
type Context struct {
	Summary   Summary
	Knowledge string
	Messages  []Message
}
type JobPayload struct {
	Edited         bool   `json:"edited,omitempty"`
	SourceUpdateID int64  `json:"source_update_id,omitempty"`
	Action         string `json:"action,omitempty"`
	Text           string `json:"text,omitempty"`
	Prompt         string `json:"prompt"`
	ThroughID      int64  `json:"through_id"`
	MessageID      int64  `json:"message_id"`
}
