package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
)

// Bounds on what the bot may propose: corrections are short behaviour notes,
// and a chat keeps only a handful, so the bot's prompt stays the main text.
const (
	CorrectionTextLimit = 300
	correctionsPerChat  = 10
)

func chatOnly(s chat.Scope) chat.Scope {
	return chat.Scope{AgentID: s.AgentID, BotID: s.BotID, ChatID: s.ChatID}
}

// Corrections lists a chat's corrections, newest first; status "" means all.
func (a *AgentStore) Corrections(ctx context.Context, s chat.Scope, status string) ([]agent.Correction, error) {
	s = chatOnly(s)
	if !s.Valid() {
		return nil, errors.New("invalid conversation scope")
	}
	q := scoped(a.db.WithContext(ctx).Table("behavior_corrections"), s)
	if status != "" {
		q = q.Where("status=?", status)
	}
	var out []agent.Correction
	return out, q.Order("id DESC").Limit(100).Find(&out).Error
}

// ProposeCorrections stores the bot's proposals for a chat, skipping exact
// duplicates of existing ones (including disabled: a rejected idea stays
// rejected). Proposals beyond the per-chat cap are dropped.
func (a *AgentStore) ProposeCorrections(ctx context.Context, s chat.Scope, proposals []agent.Correction) error {
	s = chatOnly(s)
	if !s.Valid() || len(proposals) == 0 {
		return nil
	}
	behavior, err := a.ChatBehavior(ctx, s)
	if err != nil {
		return err
	}
	status := "pending"
	if behavior.AutoApproveCorrections {
		status = "approved"
	}
	existing, err := a.Corrections(ctx, s, "")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	live := 0
	for _, c := range existing {
		seen[strings.ToLower(strings.TrimSpace(c.Text))] = true
		if c.Status != "disabled" {
			live++
		}
	}
	now := time.Now().UTC().UnixMilli()
	for _, p := range proposals {
		text := strings.TrimSpace(p.Text)
		key := strings.ToLower(text)
		if text == "" || len([]rune(text)) > CorrectionTextLimit || seen[key] || live >= correctionsPerChat {
			continue
		}
		reason := strings.TrimSpace(p.Reason)
		if r := []rune(reason); len(r) > CorrectionTextLimit {
			reason = string(r[:CorrectionTextLimit])
		}
		if err := a.db.WithContext(ctx).Exec("INSERT INTO behavior_corrections (agent_id,bot_id,chat_id,text,reason,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)", s.AgentID, s.BotID, s.ChatID, text, reason, status, now, now).Error; err != nil {
			return err
		}
		seen[key] = true
		live++
	}
	return nil
}

// ApplyReviewOperations executes a reviewer's proposed changes to the
// correction list: add new rules, replace or merge existing ones, or remove
// (disable) ones that are no longer relevant.
func (a *AgentStore) ApplyReviewOperations(ctx context.Context, s chat.Scope, ops []agent.ReviewOperation) error {
	s = chatOnly(s)
	if !s.Valid() || len(ops) == 0 {
		return nil
	}
	behavior, err := a.ChatBehavior(ctx, s)
	if err != nil {
		return err
	}
	newStatus := "pending"
	if behavior.AutoApproveCorrections {
		newStatus = "approved"
	}
	existing, err := a.Corrections(ctx, s, "")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	live := 0
	byID := map[int64]agent.Correction{}
	for _, c := range existing {
		seen[strings.ToLower(strings.TrimSpace(c.Text))] = true
		if c.Status != "disabled" {
			live++
		}
		byID[c.ID] = c
	}
	now := time.Now().UTC().UnixMilli()
	for _, op := range ops {
		switch op.Action {
		case "add":
			text := strings.TrimSpace(op.Text)
			key := strings.ToLower(text)
			if text == "" || len([]rune(text)) > CorrectionTextLimit || seen[key] || live >= correctionsPerChat {
				continue
			}
			reason := strings.TrimSpace(op.Reason)
			if r := []rune(reason); len(r) > CorrectionTextLimit {
				reason = string(r[:CorrectionTextLimit])
			}
			if err := a.db.WithContext(ctx).Exec(`INSERT INTO behavior_corrections (agent_id,bot_id,chat_id,text,reason,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
				s.AgentID, s.BotID, s.ChatID, text, reason, newStatus, now, now).Error; err != nil {
				return err
			}
			seen[key] = true
			live++
		case "replace":
			c, ok := byID[op.ID]
			if !ok || op.ID == 0 {
				continue
			}
			text := strings.TrimSpace(op.Text)
			if text == "" || len([]rune(text)) > CorrectionTextLimit {
				continue
			}
			reason := strings.TrimSpace(op.Reason)
			if r := []rune(reason); len(r) > CorrectionTextLimit {
				reason = string(r[:CorrectionTextLimit])
			}
			if err := a.db.WithContext(ctx).Exec(`UPDATE behavior_corrections SET text=?,reason=?,updated_at=?,version=version+1 WHERE agent_id=? AND bot_id=? AND chat_id=? AND id=?`,
				text, reason, now, s.AgentID, s.BotID, s.ChatID, c.ID).Error; err != nil {
				return err
			}
		case "merge":
			if len(op.IDs) < 2 {
				continue
			}
			text := strings.TrimSpace(op.Text)
			if text == "" || len([]rune(text)) > CorrectionTextLimit {
				continue
			}
			reason := strings.TrimSpace(op.Reason)
			if r := []rune(reason); len(r) > CorrectionTextLimit {
				reason = string(r[:CorrectionTextLimit])
			}
			freed := 0
			for _, id := range op.IDs {
				c, ok := byID[id]
				if !ok {
					continue
				}
				if err := a.db.WithContext(ctx).Exec(`DELETE FROM behavior_corrections WHERE agent_id=? AND bot_id=? AND chat_id=? AND id=?`,
					s.AgentID, s.BotID, s.ChatID, id).Error; err != nil {
					return err
				}
				if c.Status != "disabled" {
					freed++
				}
			}
			live -= freed
			key := strings.ToLower(text)
			if !seen[key] && live < correctionsPerChat {
				if err := a.db.WithContext(ctx).Exec(`INSERT INTO behavior_corrections (agent_id,bot_id,chat_id,text,reason,status,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
					s.AgentID, s.BotID, s.ChatID, text, reason, newStatus, now, now).Error; err != nil {
					return err
				}
				seen[key] = true
				live++
			}
		case "remove":
			c, ok := byID[op.ID]
			if !ok || op.ID == 0 {
				continue
			}
			if err := a.db.WithContext(ctx).Exec(`UPDATE behavior_corrections SET status='disabled',updated_at=?,version=version+1 WHERE agent_id=? AND bot_id=? AND chat_id=? AND id=?`,
				now, s.AgentID, s.BotID, s.ChatID, c.ID).Error; err != nil {
				return err
			}
			if c.Status != "disabled" {
				live--
			}
		}
	}
	return nil
}

// SetCorrection changes a correction's status, or deletes it ("delete").
func (a *AgentStore) SetCorrection(ctx context.Context, s chat.Scope, id, version int64, action string) error {
	s = chatOnly(s)
	if !s.Valid() || id <= 0 || version <= 0 {
		return errors.New("invalid correction")
	}
	q := scoped(a.db.WithContext(ctx).Table("behavior_corrections"), s).Where("id=? AND version=?", id, version)
	var result = q
	switch action {
	case "approve", "disable":
		status := map[string]string{"approve": "approved", "disable": "disabled"}[action]
		result = q.Updates(map[string]any{"status": status, "updated_at": time.Now().UTC().UnixMilli(), "version": version + 1})
	case "delete":
		result = q.Delete(&struct{}{})
	default:
		return errors.New("unknown correction action")
	}
	if result.Error != nil || result.RowsAffected != 1 {
		return errors.New("correction version conflict")
	}
	return nil
}
