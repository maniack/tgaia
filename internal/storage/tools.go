package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/maniack/tgaia/internal/queue"
	"gorm.io/gorm"
)

type ToolStep struct {
	Request string
	State   string
	Result  string
}

func (a *AgentStore) ToolStep(ctx context.Context, jobID int64, step string) (ToolStep, error) {
	var row ToolStep
	err := a.db.WithContext(ctx).Table("tool_steps").Where("job_id=? AND step=?", jobID, step).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, nil
	}
	if err != nil {
		return row, errors.New("reading tool checkpoint failed")
	}
	return row, nil
}

// StartTool durably records the intent BEFORE the external action. A recovered
// started intent has an unknown result and must never be executed again.
func (a *AgentStore) StartTool(ctx context.Context, job queue.Job, step, request string) (bool, error) {
	fresh := false
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		if err := (&AgentStore{db: tx}).CheckActions(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		r := tx.Exec("INSERT INTO tool_steps(job_id,step,request,state) VALUES (?,?,?,'started') ON CONFLICT DO NOTHING", job.ID, step, request)
		fresh = r.RowsAffected == 1
		return r.Error
	})
	if err != nil {
		return false, errors.New("recording tool intent failed")
	}
	return fresh, nil
}
func (a *AgentStore) FinishTool(ctx context.Context, job queue.Job, step, state, result string) error {
	if (state != "completed" && state != "uncertain") || len(result) > 32000 {
		return errors.New("invalid tool checkpoint")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		r := tx.Exec("UPDATE tool_steps SET state=?,result=?,version=version+1 WHERE job_id=? AND step=? AND state='started'", state, result, job.ID, step)
		if r.Error != nil || r.RowsAffected != 1 {
			return errors.New("tool checkpoint conflict")
		}
		return nil
	})
}
func (a *AgentStore) SaveToolTurn(ctx context.Context, job queue.Job, step, result string) error {
	if !strings.HasPrefix(step, "turn:") || len(result) > 64000 {
		return errors.New("invalid model tool turn")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO tool_steps(job_id,step,request,state,result) VALUES (?,?,?,'completed',?) ON CONFLICT DO NOTHING", job.ID, step, "", result).Error; err != nil {
			return errors.New("saving tool turn failed")
		}
		return nil
	})
}
