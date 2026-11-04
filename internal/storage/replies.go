package storage

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/queue"
)

type ReplyLink struct {
	Part      int
	MessageID int64
}

func (a *AgentStore) linkReply(ctx context.Context, job queue.Job, part int, id int64) error {
	var p agent.JobPayload
	if json.Unmarshal(job.Payload, &p) != nil {
		return errors.New("invalid reply payload")
	}
	if p.MessageID <= 0 || p.Prompt != "" {
		return nil
	}
	return a.db.WithContext(ctx).Exec(`INSERT INTO reply_links(agent_id,bot_id,chat_id,thread_id,source_message_id,part,message_id) VALUES (?,?,?,?,?,?,?) ON CONFLICT(agent_id,bot_id,chat_id,thread_id,source_message_id,part) DO UPDATE SET message_id=excluded.message_id`, job.Scope.AgentID, job.Scope.BotID, job.Scope.ChatID, job.Scope.ThreadID, p.MessageID, part, id).Error
}
func (a *AgentStore) ReplyLinks(ctx context.Context, s chat.Scope, source int64) ([]ReplyLink, error) {
	if !s.Valid() || source <= 0 {
		return nil, errors.New("invalid reply source")
	}
	var rows []ReplyLink
	err := a.db.WithContext(ctx).Raw(`SELECT r.part,r.message_id FROM reply_links r JOIN bot_messages b ON b.agent_id=r.agent_id AND b.bot_id=r.bot_id AND b.chat_id=r.chat_id AND b.message_id=r.message_id WHERE r.agent_id=? AND r.bot_id=? AND r.chat_id=? AND r.thread_id=? AND r.source_message_id=? AND b.deleted_at IS NULL ORDER BY r.part LIMIT 100`, s.AgentID, s.BotID, s.ChatID, s.ThreadID, source).Scan(&rows).Error
	return rows, err
}
