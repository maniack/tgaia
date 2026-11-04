// Package chat defines the isolation boundary shared by conversation capabilities.
package chat

import "strings"

type Scope struct {
	AgentID  string
	BotID    int64
	ChatID   int64
	ThreadID int64
}

func (s Scope) Valid() bool {
	return strings.TrimSpace(s.AgentID) != "" && len(s.AgentID) <= 128 && s.BotID > 0 && s.ChatID != 0 && s.ThreadID >= 0
}
