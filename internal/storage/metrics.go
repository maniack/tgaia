package storage

import "context"

type QueueMetric struct {
	Status string
	Count  int64
}

func (d *Database) QueueMetrics(ctx context.Context) ([]QueueMetric, error) {
	var rows []QueueMetric
	err := d.orm.WithContext(ctx).Raw("SELECT status,COUNT(*) AS count FROM queue_jobs GROUP BY status").Scan(&rows).Error
	return rows, err
}
