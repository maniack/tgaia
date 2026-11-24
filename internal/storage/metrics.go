package storage

import (
	"context"
	"time"
)

type QueueMetric struct {
	Status string
	Kind   string
	Count  int64
}

type QueueScopeMetric struct {
	AgentID         string
	BotID           int64
	ChatID          int64
	OldestPendingMs int64
}

type QueueUnhandledMetric struct {
	Kind  string
	Count int64
}

type QueueScopeHeadMetric struct {
	AgentID      string
	BotID        int64
	ChatID       int64
	Kind         string
	HeadSinceMs  int64
}

// QueueMetrics returns active job counts grouped by status and kind.
func (d *Database) QueueMetrics(ctx context.Context) ([]QueueMetric, error) {
	var rows []QueueMetric
	err := d.orm.WithContext(ctx).Raw("SELECT status,kind,COUNT(*) AS count FROM queue_jobs GROUP BY status,kind").Scan(&rows).Error
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

// QueueUnhandledMetrics returns active job counts (pending|retry|leased) for
// kinds not in the given list. Any result > 0 means a HOL-block is possible.
func (d *Database) QueueUnhandledMetrics(ctx context.Context, kinds []string) ([]QueueUnhandledMetric, error) {
	var rows []QueueUnhandledMetric
	var err error
	if len(kinds) == 0 {
		err = d.orm.WithContext(ctx).Raw(
			"SELECT kind,COUNT(*) AS count FROM queue_jobs WHERE status IN ('pending','retry','leased') GROUP BY kind",
		).Scan(&rows).Error
	} else {
		err = d.orm.WithContext(ctx).Raw(
			"SELECT kind,COUNT(*) AS count FROM queue_jobs WHERE status IN ('pending','retry','leased') AND kind NOT IN ? GROUP BY kind",
			kinds).Scan(&rows).Error
	}
	return rows, err
}

// QueueScopeHeadMetrics returns the head job of each scope that is currently
// claimable (pending/retry, available_at <= now, not behind an earlier job).
// Use the returned HeadSinceMs to compute how long the head has been waiting.
func (d *Database) QueueScopeHeadMetrics(ctx context.Context) ([]QueueScopeHeadMetric, error) {
	var rows []QueueScopeHeadMetric
	nowMs := time.Now().UnixMilli()
	err := d.orm.WithContext(ctx).Raw(`
SELECT j.agent_id,j.bot_id,j.chat_id,j.kind,
  CASE WHEN j.available_at > j.created_at THEN j.available_at ELSE j.created_at END AS head_since_ms
FROM queue_jobs j
WHERE j.status IN ('pending','retry')
  AND j.bot_id > 0
  AND j.available_at <= ?
  AND NOT EXISTS (
    SELECT 1 FROM queue_jobs e
    WHERE e.agent_id=j.agent_id AND e.bot_id=j.bot_id AND e.chat_id=j.chat_id
      AND e.id < j.id AND e.status IN ('pending','leased','retry')
  )`, nowMs).Scan(&rows).Error
	return rows, err
}
