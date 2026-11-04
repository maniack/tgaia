// Package messages owns references to the agent's outgoing Telegram messages.
package messages

import (
	"errors"
	"github.com/maniack/tgaia/internal/chat"
	"time"
)

var ErrNotFound = errors.New("bot message not found in this chat")

type Reference struct {
	Scope     chat.Scope
	MessageID int64
}
type Sent struct {
	Reference
	DeliveryKey string    // Stable originating queue key, scoped to the agent/chat.
	Part        int       // Zero-based part of a multipart response or media group.
	SentAt      time.Time // Telegram's send timestamp, not the time of a DB retry.
}
type Record struct {
	Sent
	Version   int64
	EditedAt  *time.Time
	DeletedAt *time.Time
}
type Result string

const (
	Applied   Result = "applied"
	Unchanged Result = "unchanged"
	Conflict  Result = "conflict"
)
