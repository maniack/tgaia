package storage

import (
	"context"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
)

// Transaction shares a single connection across the mutation and audit append.
func (d *Database) Transaction(ctx context.Context, fn func(*Database) error) error {
	return d.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return fn(&Database{orm: tx, sql: d.sql}) })
}
func (s *Settings) InDatabase(d *Database) *Settings {
	return &Settings{db: d.orm, cipher: s.cipher, scope: s.scope}
}
func (d *Database) Audit(ctx context.Context, subject, operation, request string, status int, states ...string) error {
	if subject == "" || len(request) > 64000 {
		return errors.New("invalid audit record")
	}
	before, after := "[]", "[]"
	if len(states) == 2 {
		before, after = states[0], states[1]
	}
	return d.orm.WithContext(ctx).Exec("INSERT INTO audit_events(subject,operation,request,status,created_at,before_state,after_state) VALUES (?,?,?,?,"+d.Queue().now+",?,?)", subject, operation, request, status, before, after).Error
}

// AuditState captures only the entity addressed by an administrative operation.
// Secret settings are represented by metadata, never ciphertext or cleartext.
func (d *Database) AuditState(ctx context.Context, path string, body map[string]any) (string, error) {
	var query string
	var args []any
	switch path {
	case "/api/user-role":
		query = "SELECT agent_id,user_id,role,version FROM seen_users WHERE agent_id=? AND user_id=?"
		args = []any{body["agent_id"], body["user_id"]}
	case "/api/user-block":
		query = "SELECT agent_id,user_id FROM blocked_users WHERE agent_id=? AND user_id=?"
		args = []any{body["agent_id"], body["user_id"]}
	case "/api/chat-block":
		query = "SELECT agent_id,bot_id,chat_id,blocked FROM chat_directory WHERE agent_id=? AND bot_id=? AND chat_id=?"
		args = []any{body["agent_id"], body["bot_id"], body["chat_id"]}
	case "/api/tasks":
		query = "SELECT * FROM scheduled_tasks WHERE id=?"
		args = []any{body["id"]}
	case "/api/settings", "/api/mcp":
		name := body["name"]
		if path == "/api/mcp" {
			name = "mcp.servers"
		}
		query = "SELECT name,CASE WHEN secret THEN '[redacted]' ELSE value END AS value,secret,version FROM runtime_settings WHERE scope=? AND name=?"
		scope := body["scope"]
		if scope == nil || scope == "" {
			scope = "shared"
		}
		args = []any{scope, name}
	case "/api/prompt":
		query = "SELECT agent_id,bot_id,system_prompt FROM telegram_bots WHERE agent_id=? AND bot_id=?"
		args = []any{body["agent_id"], body["bot_id"]}
	case "/api/knowledge":
		query = "SELECT * FROM shared_knowledge WHERE agent_id=?"
		args = []any{body["agent_id"]}
	case "/api/memory":
		query = "SELECT agent_id,bot_id,chat_id,source_message_id,created_by,content,created_at FROM explicit_memories WHERE agent_id=? AND bot_id=? AND chat_id=? AND source_message_id=?"
		args = []any{body["agent_id"], body["bot_id"], body["chat_id"], body["source_message_id"]}
	case "/api/resolve-outcome":
		query = "SELECT id,job_id,target,action,subject,reason,created_at FROM outcome_resolutions WHERE job_id=? AND target=?"
		args = []any{body["job_id"], body["target"]}
	case "/api/queue-replay":
		query = "SELECT id,kind,status,attempts,max_attempts FROM queue_jobs WHERE id=?"
		args = []any{body["job_id"]}
	default:
		return "[]", nil
	}
	var rows []map[string]any
	if err := d.orm.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return "", err
	}
	data, err := json.Marshal(rows)
	return string(data), err
}
