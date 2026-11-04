package service

import (
	"context"
	"github.com/maniack/tgaia/internal/queue"
)

func (e *Engine) reserveModel(ctx context.Context, j queue.Job, compaction bool) error {
	if err := e.DB.Agent().Check(ctx, j.Scope, j.ActorUserID); err != nil {
		return err
	}
	if err := e.DB.Agent().CheckLease(ctx, j); err != nil {
		return err
	}
	kind, key := "llm", "llm_calls_per_chat_hour"
	if compaction {
		kind, key = "compaction", "compaction_calls_per_chat_hour"
	}
	return e.DB.Agent().ReserveCall(ctx, j.Scope, kind, e.Config.Limit(key))
}
