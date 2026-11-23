package storage

import "context"

type QueueMetric struct {
	Status string
	Count  int64
}

type QueueScopeMetric struct {
	AgentID         string
	BotID           int64
	ChatID          int64
	OldestPendingMs int64
}

func (d *Database) QueueMetrics(ctx context.Context) ([]QueueMetric, error) {
	var rows []QueueMetric
	err := d.orm.WithContext(ctx).Raw("SELECT status,COUNT(*) AS count FROM queue_jobs GROUP BY status").Scan(&rows).Error
	return rows, err
}

// QueueScopeMetrics returns the oldest pending job timestamp per active scope,
// used to compute tgaia_queue_oldest_pending_seconds in the /metrics handler.
func (d *Database) QueueScopeMetrics(ctx context.Context) ([]QueueScopeMetric, error) {
	var rows []QueueScopeMetric
	err := d.orm.WithContext(ctx).Raw(`SELECT agent_id,bot_id,chat_id,MIN(available_at) AS oldest_pending_ms
 FROM queue_jobs WHERE status='pending' GROUP BY agent_id,bot_id,chat_id`).Scan(&rows).Error
	return rows, err
}
