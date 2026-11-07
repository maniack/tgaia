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
	LanguageCode   string
	IsPremium      bool
	IsBot          bool
	ChatType       string
	Title          string
	IsForum        bool
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
	IsBot    bool   `json:"is_bot,omitempty"`
	// ReplyToMessageID is the message this one replied to (0 when none).
	ReplyToMessageID int64 `json:"reply_to_message_id"`
	// Quoted is the replied-to message when it lies outside the loaded window;
	// filled by the store, never persisted.
	Quoted string `json:"-" gorm:"-"`
}

// Header formats the prompt header of a history line: role, IDs, sender and
// the reply target, with the quoted text when it is not otherwise visible.
func (m Message) Header() string {
	h := fmt.Sprintf("[%s message_id=%d from=%s sender_chat=%d", m.Role, m.MessageID, m.Who(), m.SenderChatID)
	if m.IsBot {
		h += " is_bot=true"
	}
	if m.ReplyToMessageID > 0 {
		h += fmt.Sprintf(" reply_to=%d", m.ReplyToMessageID)
	}
	h += "]"
	if m.Quoted != "" {
		h += " (quoted message " + m.Quoted + ")"
	}
	return h
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
	IsForum  bool   `json:"is_forum"`
	Blocked  bool   `json:"blocked"`
}
type Summary struct {
	Content      string `json:"content"`
	ThroughID    int64  `json:"through_id"`
	ThroughAt    int64  `json:"through_at" gorm:"-"`
	PendingCount int64  `json:"pending_count" gorm:"-"`
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

// ProfileMemory is an explicit memory attributed to one person, with enough
// conversation identity for administrators to inspect or remove its source.
type ProfileMemory struct {
	AgentID         string `json:"agent_id"`
	BotID           int64  `json:"bot_id"`
	ChatID          int64  `json:"chat_id"`
	ThreadID        int64  `json:"thread_id"`
	ChatTitle       string `json:"chat_title"`
	SourceMessageID int64  `json:"source_message_id"`
	Content         string `json:"content"`
	CreatedAt       int64  `json:"created_at"`
}

type TelegramProfile struct {
	AgentID      string `json:"agent_id"`
	UserID       int64  `json:"user_id"`
	Username     string `json:"username"`
	Name         string `json:"name"`
	LanguageCode string `json:"language_code"`
	IsPremium    bool   `json:"is_premium"`
	IsBot        bool   `json:"is_bot"`
}

type ChatBehavior struct {
	AgentID        string `json:"agent_id"`
	BotID          int64  `json:"bot_id"`
	ChatID         int64  `json:"chat_id"`
	GroupResponse  string `json:"group_response"`
	RespondToBots  bool   `json:"respond_to_bots"`
	LoopProtection bool   `json:"loop_protection"`
	// ChatToolRounds and ChatToolCalls override the global tool_rounds and
	// tool_calls limits for this chat. Zero means "use the global default".
	ChatToolRounds int  `json:"chat_tool_rounds"`
	ChatToolCalls  int  `json:"chat_tool_calls"`
	// AllowSkip lets the model reply with skip:true to suppress sending a
	// message when there is nothing meaningful to say.
	AllowSkip bool  `json:"allow_skip"`
	Version   int64 `json:"version"`
}

// ReactionUpdate is the current reaction set reported by Telegram for one
// actor and one bot-authored message. An empty Reactions slice removes that
// actor's previous reactions. It is passive feedback and never creates a job.
type ReactionUpdate struct {
	AgentID       string
	BotID         int64
	ChatID        int64
	MessageID     int64
	UpdateID      int64
	ActorType     string
	ActorID       int64
	ActorName     string
	ActorUsername string
	ActorIsBot    bool
	Reactions     []Reaction
	UpdatedAt     time.Time
}

type Reaction struct {
	Type  string
	Value string
	Count int
}

// MessageReaction is passive feedback supplied to the model on a later
// message. MessageText is a bounded preview of the bot message it targeted.
type MessageReaction struct {
	MessageID     int64
	ActorType     string
	ActorID       int64
	ActorName     string
	ActorUsername string
	ActorIsBot    bool
	Type          string
	Value         string
	Count         int
	UpdatedAt     int64
	MessageText   string
}

func (m ExplicitMemory) Author() string {
	return who(m.CreatedBy, m.CreatedByName, m.CreatedByUsername)
}

type Context struct {
	Summary          Summary
	Knowledge        string
	ExplicitMemories []ExplicitMemory
	Reactions        []MessageReaction
	Messages         []Message
}
type JobPayload struct {
	Edited         bool   `json:"edited,omitempty"`
	SourceUpdateID int64  `json:"source_update_id,omitempty"`
	FailureClass   string `json:"failure_class,omitempty"`
	Action         string `json:"action,omitempty"`
	Text           string `json:"text,omitempty"`
	Prompt         string `json:"prompt"`
	ThroughID      int64  `json:"through_id"`
	MessageID      int64  `json:"message_id"`
}
