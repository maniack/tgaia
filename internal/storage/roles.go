package storage

import (
	"context"
	"errors"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/queue"
	"gorm.io/gorm"
)

// CheckActions always reads current policy; model instructions cannot grant access.
func (a *AgentStore) CheckActions(ctx context.Context, s chat.Scope, userID int64) error {
	if err := a.Check(ctx, s, userID); err != nil {
		return err
	}
	if userID <= 0 {
		return access.ErrDenied
	}
	var row struct{ Role string }
	err := a.db.WithContext(ctx).Table("seen_users").Where("agent_id=? AND user_id=?", s.AgentID, userID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} // default user
	if err != nil {
		return err
	}
	if row.Role != "user" {
		return access.ErrDenied
	}
	return nil
}

func (a *AgentStore) SetRole(ctx context.Context, agentID string, userID int64, role string, expected int64) error {
	if !validAgent(agentID) || userID <= 0 || expected <= 0 || (role != "reader" && role != "user") {
		return errors.New("invalid role assignment")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		r := tx.Exec("UPDATE seen_users SET role=?,version=version+1 WHERE agent_id=? AND user_id=? AND version=?", role, agentID, userID, expected)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("role version conflict")
		}
		if role == "reader" {
			// Retire work accepted with older capabilities. Unblocking/promotion never replays it.
			if err := retireUser(tx, agentID, userID); err != nil {
				return err
			}
		}
		return nil
	})
}
func retireUser(tx *gorm.DB, agentID string, userID int64) error {
	if err := tx.Exec("UPDATE scheduled_tasks SET active=?,version=version+1 WHERE agent_id=? AND user_id=? AND active=?", false, agentID, userID, true).Error; err != nil {
		return err
	}
	return tx.Exec("UPDATE queue_jobs SET status='dead',lease_token='',lease_until=0 WHERE agent_id=? AND actor_user_id=? AND status IN ('pending','retry','leased')", agentID, userID).Error
}

func (a *AgentStore) CheckLease(ctx context.Context, job queue.Job) error {
	return fence(a.db.WithContext(ctx), job)
}
