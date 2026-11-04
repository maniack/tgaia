package storage

import (
	"context"
	"errors"
)

func (a *AgentStore) PollLease(ctx context.Context, agentID string, botID int64, token string) (int64, bool, error) {
	db := a.db.WithContext(ctx)
	q := (&Database{orm: a.db}).Queue()
	if err := db.Exec("INSERT INTO bot_poll_leases (agent_id,bot_id) VALUES (?,?) ON CONFLICT DO NOTHING", agentID, botID).Error; err != nil {
		return 0, false, errors.New("poll lease initialization failed")
	}
	var row struct{ OffsetID int64 }
	r := db.Raw("UPDATE bot_poll_leases SET token=?,lease_until="+q.now+"+60000 WHERE agent_id=? AND bot_id=? AND (lease_until<="+q.now+" OR token=?) RETURNING offset_id", token, agentID, botID, token).Scan(&row)
	if r.Error != nil {
		return 0, false, errors.New("poll lease claim failed")
	}
	return row.OffsetID, r.RowsAffected == 1, nil
}
func (a *AgentStore) PollAdvance(ctx context.Context, agentID string, botID int64, token string, offset int64) error {
	q := (&Database{orm: a.db}).Queue()
	r := a.db.WithContext(ctx).Exec("UPDATE bot_poll_leases SET offset_id=? WHERE agent_id=? AND bot_id=? AND token=? AND lease_until>"+q.now+" AND offset_id<=?", offset, agentID, botID, token, offset)
	if r.Error != nil || r.RowsAffected != 1 {
		return errors.New("poll lease lost")
	}
	return nil
}

func (a *AgentStore) PollRelease(ctx context.Context, agentID string, botID int64, token string) error {
	if err := a.db.WithContext(ctx).Exec("UPDATE bot_poll_leases SET token='',lease_until=0 WHERE agent_id=? AND bot_id=? AND token=?", agentID, botID, token).Error; err != nil {
		return errors.New("poll lease release failed")
	}
	return nil
}
