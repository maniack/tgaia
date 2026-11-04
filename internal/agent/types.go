package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/chat"
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
	// Sender identity as last seen (seen_users), so the model can tell people apart.
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Who formats a sender for prompts: name, @username and the stable user ID.
func (m Message) Who() string { return who(m.UserID, m.Name, m.Username) }

func who(id int64, name, username string) string {
	parts := make([]string, 0, 3)
	if name != "" {
		parts = append(parts, fmt.Sprintf("%q", name))
	}
	if username != "" {
		parts = append(parts, "@"+username)
	}
	parts = append(parts, fmt.Sprintf("id=%d", id))
	return strings.Join(parts, " ")
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
type ExplicitMemory struct {
	SourceMessageID int64  `json:"source_message_id"`
	SourceUpdateID  int64  `json:"source_update_id"`
	CreatedBy       int64  `json:"created_by"`
	Content         string `json:"content"`
	EmbeddingModel  string `json:"-"`
	Embedding       string `json:"-"`
	CreatedAt       int64  `json:"created_at"`
	// Author identity as last seen, for unambiguous attribution in prompts.
	CreatedByUsername string `json:"created_by_username,omitempty"`
	CreatedByName     string `json:"created_by_name,omitempty"`
}

func (m ExplicitMemory) Author() string {
	return who(m.CreatedBy, m.CreatedByName, m.CreatedByUsername)
}

type Context struct {
	Summary          Summary
	Knowledge        string
	ExplicitMemories []ExplicitMemory
	Messages         []Message
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
