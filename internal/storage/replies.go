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

// RespondSucceeded reports whether a respond job for the given source message
// reached completed status with a stored result. When false, the original job
// was never successfully answered (it died or only sent a failure notice), so
// an edited version of the message should be treated as a new request with tools.
func (a *AgentStore) RespondSucceeded(ctx context.Context, s chat.Scope, messageID int64) (bool, error) {
	if !s.Valid() || messageID <= 0 {
		return false, errors.New("invalid scope or message id")
	}
	msgIDExpr := "CAST(json_extract(j.payload,'$.message_id') AS BIGINT)"
	if a.db.Dialector.Name() == "postgres" {
		msgIDExpr = "CAST(j.payload::jsonb->>'message_id' AS BIGINT)"
	}
	var count int64
	err := a.db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FROM queue_jobs j JOIN job_results r ON r.job_id=j.id
		 WHERE j.agent_id=? AND j.bot_id=? AND j.chat_id=? AND j.kind='respond'
		 AND j.status='completed' AND `+msgIDExpr+`=?`,
		s.AgentID, s.BotID, s.ChatID, messageID,
	).Scan(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
