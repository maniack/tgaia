package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/messages"
	"gorm.io/gorm"
)

type BotMessages struct{ db *gorm.DB }

func (d *Database) BotMessages() *BotMessages { return &BotMessages{db: d.orm} }

type messageRow struct {
	AgentID     string
	BotID       int64
	ChatID      int64
	ThreadID    int64
	MessageID   int64
	DeliveryKey string
	Part        int
	SentAt      int64
	Version     int64
	EditedAt    *int64
	DeletedAt   *int64
}

func (messageRow) TableName() string { return "bot_messages" }
func (r messageRow) record() messages.Record {
	result := messages.Record{Sent: messages.Sent{Reference: messages.Reference{Scope: chat.Scope{AgentID: r.AgentID, BotID: r.BotID, ChatID: r.ChatID, ThreadID: r.ThreadID}, MessageID: r.MessageID}, DeliveryKey: r.DeliveryKey, Part: r.Part, SentAt: time.UnixMilli(r.SentAt).UTC()}, Version: r.Version}
	if r.EditedAt != nil {
		at := time.UnixMilli(*r.EditedAt).UTC()
		result.EditedAt = &at
	}
	if r.DeletedAt != nil {
		at := time.UnixMilli(*r.DeletedAt).UTC()
		result.DeletedAt = &at
	}
	return result
}
func validReference(ref messages.Reference) bool { return ref.Scope.Valid() && ref.MessageID > 0 }
func (m *BotMessages) reference(ctx context.Context, ref messages.Reference) *gorm.DB {
	return m.db.WithContext(ctx).Model(&messageRow{}).Where("agent_id=? AND bot_id=? AND chat_id=? AND thread_id=? AND message_id=?", ref.Scope.AgentID, ref.Scope.BotID, ref.Scope.ChatID, ref.Scope.ThreadID, ref.MessageID)
}

// RecordSent persists an actual Telegram response, including each multipart ID.
// A duplicate delivery may have a new message ID: retain BOTH, don't hide it.
func (m *BotMessages) RecordSent(ctx context.Context, sent messages.Sent) (messages.Result, error) {
	if !validReference(sent.Reference) || strings.TrimSpace(sent.DeliveryKey) == "" || len(sent.DeliveryKey) > 256 || sent.Part < 0 || sent.Part > 2147483647 || sent.SentAt.IsZero() {
		return "", errors.New("invalid sent message reference")
	}
	result := m.db.WithContext(ctx).Exec(`INSERT INTO bot_messages (agent_id,bot_id,chat_id,thread_id,message_id,delivery_key,part,sent_at)
 VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (agent_id,bot_id,chat_id,message_id) DO NOTHING`, sent.Scope.AgentID, sent.Scope.BotID, sent.Scope.ChatID, sent.Scope.ThreadID, sent.MessageID, sent.DeliveryKey, sent.Part, sent.SentAt.UnixMilli())
	if result.Error != nil {
		return "", errors.New("recording bot message failed")
	}
	if result.RowsAffected == 1 {
		return messages.Applied, nil
	}
	existing, err := m.Get(ctx, sent.Reference)
	if err != nil {
		return "", err
	}
	if existing.DeliveryKey != sent.DeliveryKey || existing.Part != sent.Part || existing.SentAt.UnixMilli() != sent.SentAt.UnixMilli() {
		return messages.Conflict, nil
	}
	return messages.Unchanged, nil
}
func (m *BotMessages) Get(ctx context.Context, ref messages.Reference) (messages.Record, error) {
	if !validReference(ref) {
		return messages.Record{}, errors.New("invalid message reference")
	}
	var row messageRow
	err := m.reference(ctx, ref).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return messages.Record{}, messages.ErrNotFound
	}
	if err != nil {
		return messages.Record{}, errors.New("reading bot message failed")
	}
	return row.record(), nil
}

// List is ordered by the unique Telegram message ID within one explicit scope.
// afterMessageID=0 starts the first page. An optional deliveryKey selects all
// known messages for one response/job, including ambiguous-send duplicates.
func (m *BotMessages) List(ctx context.Context, scope chat.Scope, deliveryKey string, afterMessageID int64, limit int) ([]messages.Record, error) {
	if !scope.Valid() || len(deliveryKey) > 256 || afterMessageID < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("invalid bot message query")
	}
	query := m.db.WithContext(ctx).Where("agent_id=? AND bot_id=? AND chat_id=? AND thread_id=? AND message_id>?", scope.AgentID, scope.BotID, scope.ChatID, scope.ThreadID, afterMessageID)
	if deliveryKey != "" {
		query = query.Where("delivery_key=?", deliveryKey)
	}
	var rows []messageRow
	if err := query.Order("message_id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, errors.New("listing bot messages failed")
	}
	records := make([]messages.Record, 0, len(rows))
	for _, row := range rows {
		records = append(records, row.record())
	}
	return records, nil
}

// ConfirmEdited/ConfirmDeleted record confirmed external outcomes only. They
// don't call Telegram or authorize an action. Version checks protect DB state,
// not in-flight external effects; handlers must serialize mutations per chat.
func (m *BotMessages) ConfirmEdited(ctx context.Context, ref messages.Reference, version int64, at time.Time) (messages.Result, error) {
	return m.confirm(ctx, ref, version, at, false)
}
func (m *BotMessages) ConfirmDeleted(ctx context.Context, ref messages.Reference, version int64, at time.Time) (messages.Result, error) {
	return m.confirm(ctx, ref, version, at, true)
}
func (m *BotMessages) confirm(ctx context.Context, ref messages.Reference, version int64, at time.Time, deleted bool) (messages.Result, error) {
	if !validReference(ref) || version < 1 || at.IsZero() {
		return "", errors.New("invalid bot message mutation")
	}
	column := "edited_at"
	if deleted {
		column = "deleted_at"
	}
	result := m.reference(ctx, ref).Where("version=? AND deleted_at IS NULL AND sent_at<=? AND (edited_at IS NULL OR edited_at<=?)", version, at.UnixMilli(), at.UnixMilli()).Updates(map[string]any{column: at.UnixMilli(), "version": gorm.Expr("version+1")})
	if result.Error != nil {
		return "", errors.New("updating bot message failed")
	}
	if result.RowsAffected == 1 {
		return messages.Applied, nil
	}
	existing, err := m.Get(ctx, ref)
	if err != nil {
		return "", err
	}
	if deleted && existing.DeletedAt != nil {
		return messages.Unchanged, nil
	}
	return messages.Conflict, nil
}
