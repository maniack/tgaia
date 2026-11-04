package storage

import (
	"context"
	"errors"
	"github.com/maniack/tgaia/internal/chat"
)

var ErrQuota = errors.New("configured quota exhausted")

// Fixed UTC-hour counters are shared by all replicas and bounded to one row per chat/category.
func (a *AgentStore) ReserveCall(ctx context.Context, s chat.Scope, category string, limit int) error {
	if limit == 0 {
		return nil
	}
	if limit < 0 || !s.Valid() || (category != "llm" && category != "compaction") {
		return errors.New("invalid quota")
	}
	now := (&Database{orm: a.db}).Queue().now
	bucket := "CAST((" + now + ") / 3600000 AS BIGINT)"
	r := a.db.WithContext(ctx).Exec(`INSERT INTO usage_quotas(agent_id,bot_id,chat_id,category,bucket,used) VALUES (?,?,?,?,`+bucket+`,1)
 ON CONFLICT(agent_id,bot_id,chat_id,category) DO UPDATE SET bucket=excluded.bucket,used=CASE WHEN usage_quotas.bucket=excluded.bucket THEN usage_quotas.used+1 ELSE 1 END
 WHERE usage_quotas.bucket<>excluded.bucket OR usage_quotas.used<?`, s.AgentID, s.BotID, s.ChatID, category, limit)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrQuota
	}
	return nil
}
