package service

import (
	"context"
	"errors"

	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/storage"
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
	err := e.DB.Agent().ReserveCall(ctx, j.Scope, kind, e.Config.Limit(key))
	if errors.Is(err, storage.ErrQuota) {
		// Quotas reset on the next fixed hour; ordinary retries wait for it.
		return queue.Fail("quota", false, err)
	}
	return err
}
