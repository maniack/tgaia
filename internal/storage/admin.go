package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var ErrResolutionConflict = errors.New("outcome changed, already resolved, or conversation has active work; refresh and retry")

type Page struct {
	Items []map[string]any `json:"items"`
	Next  string           `json:"next_cursor"`
}
type pageCursor struct {
	Kind   string
	Scope  chat.Scope
	Values []string
}

// AdminPage uses an allowlisted query and a stable composite key, never SQL supplied by clients.
func (a *AgentStore) AdminPage(ctx context.Context, kind, cursor string, limit int, scope chat.Scope) (Page, error) {
	result := Page{Items: []map[string]any{}}
	if limit < 1 || limit > 100 || len(cursor) > 4096 {
		return result, errors.New("invalid page")
	}
	var query, keys string
	descending := false
	args := []any{}
	switch kind {
	case "audit":
		query = "SELECT * FROM audit_events WHERE 1=1"
		keys = "id"
		descending = true
	case "bots":
		query = "SELECT agent_id,bot_id FROM telegram_bots WHERE bot_id>0"
		keys = "agent_id,bot_id"
	case "chats":
		query = "SELECT c.*,t.thread_id FROM chat_directory c JOIN chat_topics t ON c.agent_id=t.agent_id AND c.bot_id=t.bot_id AND c.chat_id=t.chat_id WHERE c.bot_id>0 AND (c.is_forum=TRUE OR t.thread_id=0)"
		keys = "agent_id,bot_id,chat_id,thread_id"
	case "users":
		query = "SELECT u.agent_id,u.user_id,u.username,u.name,u.role,u.version,CASE WHEN b.user_id IS NULL THEN FALSE ELSE TRUE END AS blocked FROM seen_users u LEFT JOIN blocked_users b ON u.agent_id=b.agent_id AND u.user_id=b.user_id WHERE 1=1"
		keys = "agent_id,user_id"
	case "tasks":
		query = "SELECT * FROM scheduled_tasks WHERE 1=1"
		keys = "run_at,id"
	case "deliveries":
		query = "SELECT d.*,j.agent_id,j.bot_id,j.chat_id,j.thread_id,r.action AS resolution FROM outbound_deliveries d JOIN queue_jobs j ON j.id=d.job_id LEFT JOIN outcome_resolutions r ON r.job_id=d.job_id AND r.target=('delivery:' || CAST(d.part AS TEXT)) WHERE 1=1"
		keys = "job_id,part"
		descending = true
	case "tools":
		query = "SELECT t.*,j.agent_id,j.bot_id,j.chat_id,j.thread_id,r.action AS resolution FROM tool_steps t JOIN queue_jobs j ON j.id=t.job_id LEFT JOIN outcome_resolutions r ON r.job_id=t.job_id AND r.target=t.step WHERE t.step LIKE 'call:%'"
		keys = "job_id,step"
		descending = true
	case "history":
		if !scope.Valid() {
			return result, errors.New("history requires a valid scope")
		}
		query = "SELECT m.* FROM conversation_messages m JOIN chat_directory c ON c.agent_id=m.agent_id AND c.bot_id=m.bot_id AND c.chat_id=m.chat_id WHERE m.agent_id=? AND m.bot_id=? AND m.chat_id=? AND (c.is_forum=FALSE OR m.thread_id=?)"
		args = []any{scope.AgentID, scope.BotID, scope.ChatID, scope.ThreadID}
		keys = "id"
		descending = true
	default:
		return result, errors.New("unknown list")
	}
	// Wrapping gives joined queries unambiguous key names.
	query = "SELECT * FROM (" + query + ") AS page_rows WHERE 1=1"
	columns := strings.Split(keys, ",")
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		var c pageCursor
		if err != nil || json.Unmarshal(raw, &c) != nil || c.Kind != kind || c.Scope != scope || len(c.Values) != len(columns) {
			return result, errors.New("invalid cursor")
		}
		op := ">"
		if descending {
			op = "<"
		}
		// Lexicographic predicates work for a single key and composite keys on both adapters.
		terms := []string{}
		for i, col := range columns {
			parts := []string{}
			for j := 0; j < i; j++ {
				parts = append(parts, columns[j]+"=?")
				args = append(args, c.Values[j])
			}
			parts = append(parts, col+op+"?")
			args = append(args, c.Values[i])
			terms = append(terms, "("+strings.Join(parts, " AND ")+")")
		}
		query += " AND (" + strings.Join(terms, " OR ") + ")"
	}
	orders := []string{}
	for _, col := range columns {
		if descending {
			col += " DESC"
		}
		orders = append(orders, col)
	}
	query += " ORDER BY " + strings.Join(orders, ",") + " LIMIT ?"
	args = append(args, limit+1)
	if err := a.db.WithContext(ctx).Raw(query, args...).Scan(&result.Items).Error; err != nil {
		return result, errors.New("loading page failed")
	}
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		last := result.Items[limit-1]
		values := []string{}
		for _, col := range columns {
			values = append(values, fmt.Sprint(last[col]))
		}
		raw, _ := json.Marshal(pageCursor{kind, scope, values})
		result.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

type Resolution struct {
	JobID     int64  `json:"job_id"`
	Target    string `json:"target"` // delivery:<part> or call:<round>:<index>
	Version   int64  `json:"version"`
	Action    string `json:"action"` // confirm or dismiss; never blindly retry
	MessageID int64  `json:"message_id"`
	SentAt    int64  `json:"sent_at"`
	Result    string `json:"result"`
	Reason    string `json:"reason"`
}

// ResolveOutcome records an administrator's external verification. It never invokes
// Telegram/MCP itself and never deletes checkpoints to repeat an uncertain action.
func (a *AgentStore) ResolveOutcome(ctx context.Context, in Resolution, subject string) error {
	if in.JobID <= 0 || in.Version < 1 || (in.Action != "confirm" && in.Action != "dismiss") || strings.TrimSpace(subject) == "" || len(subject) > 512 || strings.TrimSpace(in.Reason) == "" || len(in.Reason) > 2000 {
		return errors.New("invalid resolution")
	}
	delivery := strings.HasPrefix(in.Target, "delivery:")
	part, err := strconv.Atoi(strings.TrimPrefix(in.Target, "delivery:"))
	if delivery && (err != nil || part < 0 || in.Target != fmt.Sprintf("delivery:%d", part)) {
		return errors.New("invalid delivery target")
	}
	if !delivery && (!strings.HasPrefix(in.Target, "call:") || len(in.Target) > 80) {
		return errors.New("invalid tool target")
	}
	if in.Action == "confirm" {
		if delivery && (in.MessageID <= 0 || in.SentAt <= 0 || in.SentAt > time.Now().Add(time.Minute).UnixMilli()) {
			return errors.New("confirmed delivery requires Telegram message ID and send time")
		}
		if !delivery && (len(in.Result) == 0 || len(in.Result) > 32000 || !json.Valid([]byte(in.Result))) {
			return errors.New("confirmed tool result must be JSON, at most 32000 bytes")
		}
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job struct {
			AgentID                              string
			BotID, ChatID, ThreadID, ActorUserID int64
			Status, Kind, Payload                string
		}
		if err := tx.Table("queue_jobs").Where("id=?", in.JobID).Take(&job).Error; err != nil {
			return ErrResolutionConflict
		}
		if job.Kind != "respond" {
			return errors.New("unsupported job kind")
		}
		sc := chat.Scope{AgentID: job.AgentID, BotID: job.BotID, ChatID: job.ChatID, ThreadID: job.ThreadID}
		// Serialize against Enqueue/Claim via the same scope-row write lock.
		if err := scoped(tx.Table("queue_scopes"), sc).Update("chat_id", job.ChatID).Error; err != nil {
			return err
		}
		if tx.Dialector.Name() == "postgres" {
			var locked []struct{ ID int64 }
			if err := scoped(tx.Table("queue_jobs"), sc).Where("status IN ('pending','retry','leased') OR id=?", in.JobID).Clauses(clause.Locking{Strength: "UPDATE"}).Find(&locked).Error; err != nil {
				return err
			}
		}
		q := (&Database{orm: tx}).Queue()
		var active int64
		if err := scoped(tx.Table("queue_jobs"), sc).Where("status='leased' AND lease_until>" + q.now).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return ErrResolutionConflict
		}
		var exists int64
		if err := tx.Table("outcome_resolutions").Where("job_id=? AND target=?", in.JobID, in.Target).Count(&exists).Error; err != nil {
			return err
		}
		if exists > 0 {
			return ErrResolutionConflict
		}
		if delivery {
			var row struct {
				State, Text string
				Version     int64
			}
			if err := tx.Table("outbound_deliveries").Where("job_id=? AND part=?", in.JobID, part).Take(&row).Error; err != nil || row.Version != in.Version || (row.State != "sending" && row.State != "uncertain") {
				return ErrResolutionConflict
			}
			state := "uncertain"
			if in.Action == "confirm" {
				state = "sent"

				outcome, err := (&BotMessages{db: tx}).RecordSent(ctx, messages.Sent{Reference: messages.Reference{Scope: sc, MessageID: in.MessageID}, DeliveryKey: fmt.Sprintf("job:%d", in.JobID), Part: part, SentAt: time.UnixMilli(in.SentAt)})
				if err != nil {
					return err
				}
				if outcome == messages.Conflict {
					return ErrResolutionConflict
				}
				if err := (&AgentStore{db: tx}).linkReply(ctx, queue.Job{Receipt: queue.Receipt{ID: in.JobID}, Scope: sc, Payload: []byte(job.Payload)}, part, in.MessageID); err != nil {
					return err
				}
				// Do not silently associate an existing different history message with this receipt.
				var count int64
				if err := scoped(tx.Table("conversation_messages"), sc).Where("message_id=?", in.MessageID).Count(&count).Error; err != nil {
					return err
				}
				if count > 0 {
					return ErrResolutionConflict
				}
				if err := tx.Exec("INSERT INTO conversation_messages (agent_id,bot_id,chat_id,thread_id,message_id,user_id,role,text,sent_at) VALUES (?,?,?,?,?,0,'assistant',?,?)", sc.AgentID, sc.BotID, sc.ChatID, sc.ThreadID, in.MessageID, row.Text, in.SentAt).Error; err != nil {
					return err
				}
			}
			r := tx.Exec("UPDATE outbound_deliveries SET state=?,message_id=?,version=version+1 WHERE job_id=? AND part=? AND version=?", state, nullableMessageID(in), in.JobID, part, in.Version)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrResolutionConflict
			}
		} else {
			result := in.Result
			state := "completed"
			if in.Action == "dismiss" {
				state = "uncertain"
				result = `{"is_error":true,"content":"Administrator closed this unknown outcome without confirming success. Do not repeat the action."}`
			}
			r := tx.Exec("UPDATE tool_steps SET state=?,result=?,version=version+1 WHERE job_id=? AND step=? AND version=? AND state IN ('started','uncertain')", state, result, in.JobID, in.Target, in.Version)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrResolutionConflict
			}
		}
		if err := tx.Exec("INSERT INTO outcome_resolutions (job_id,target,action,subject,reason,created_at) VALUES (?,?,?,?,?,"+q.now+")", in.JobID, in.Target, in.Action, subject, in.Reason).Error; err != nil {
			return err
		}
		// Resume remaining response work only when necessary. A tool resolution never
		// regenerates an already delivered/cached final answer or repeats a tool call.
		resume := delivery
		if !delivery {
			var count int64
			if err := tx.Table("job_results").Where("job_id=?", in.JobID).Count(&count).Error; err != nil {
				return err
			}
			resume = count == 0
		}
		if resume {
			if err := (&AgentStore{db: tx}).Check(ctx, sc, job.ActorUserID); err != nil {
				return err
			}
			if err := tx.Exec("UPDATE queue_jobs SET status='pending',attempts=0,lease_token='',lease_until=0,available_at="+q.now+",updated_at="+q.now+" WHERE id=?", in.JobID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
func nullableMessageID(in Resolution) any {
	if in.Action == "confirm" {
		return in.MessageID
	}
	return nil
}
