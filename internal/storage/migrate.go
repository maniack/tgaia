package storage

import (
	"context"
	"embed"
	"errors"
	"strings"

	"gorm.io/gorm"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate is explicit: normal startup never changes the schema. In Kubernetes
// run this as a migration Job before rolling out consumers.
func (d *Database) Migrate(ctx context.Context) error { return d.migrate(ctx, 43) }

func (d *Database) migrate(ctx context.Context, target int) error {
	err := d.orm.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(814728541)").Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)").Error; err != nil {
			return err
		}
		for index, path := range []string{"migrations/001_queue.sql", "migrations/002_bot_messages.sql", "migrations/003_bots_and_access.sql", "migrations/004_poc.sql", "migrations/005_settings.sql", "migrations/006_polling.sql", "migrations/007_runtime_identity.sql", "migrations/008_memory_revision.sql", "migrations/009_mcp.sql", "migrations/010_admin.sql", "migrations/011_telegram.sql", "migrations/012_reply_links.sql", "migrations/013_roles.sql", "migrations/014_audit.sql", "migrations/015_quotas.sql", "migrations/016_setting_scopes.sql", "migrations/017_explicit_memory.sql", "migrations/018_chat_memory.sql", "migrations/019_reply_to.sql", "migrations/020_forum_state.sql", "migrations/021_user_telegram_profile.sql", "migrations/022_skills_vfs.sql", "migrations/023_message_reactions.sql", "migrations/024_bot_behavior.sql", "migrations/025_failure_class.sql", "migrations/026_chat_tool_limits.sql", "migrations/027_allow_skip.sql", "migrations/028_bot_tool_limits.sql", "migrations/029_behavior_corrections.sql", "migrations/030_compaction_count.sql", "migrations/031_fix_supergroup_thread_ids.sql", "migrations/032_choice_sessions.sql", "migrations/033_bot_avatar_cache.sql", "migrations/034_queue_priority.sql", "migrations/035_task_limits.sql", "migrations/036_vfs_chat_only.sql", "migrations/037_queue_cancel.sql", "migrations/038_queue_paused_scopes.sql", "migrations/039_queue_phase.sql", "migrations/040_skill_pending.sql", "migrations/041_memory_episodes.sql", "migrations/042_unified_memory.sql", "migrations/043_drop_v3_status_check.sql"} {
			version := index + 1
			if version > target {
				break
			}
			var count int64
			if err := tx.Table("schema_migrations").Where("version = ?", version).Count(&count).Error; err != nil {
				return err
			}
			if count != 0 {
				continue
			}
			data, err := migrations.ReadFile(path)
			if err != nil {
				return err
			}
			id := "INTEGER PRIMARY KEY AUTOINCREMENT"
			if tx.Dialector.Name() == "postgres" {
				id = "BIGSERIAL PRIMARY KEY"
			}
			sourceMessageID := "COALESCE(CAST(json_extract(j.payload,'$.message_id') AS BIGINT),0)"
			if tx.Dialector.Name() == "postgres" {
				sourceMessageID = "COALESCE(CAST(j.payload::jsonb->>'message_id' AS BIGINT),0)"
			}
			data = []byte(strings.ReplaceAll(string(data), "{{SOURCE_MESSAGE_ID}}", sourceMessageID))
			ddl := strings.ReplaceAll(string(data), "{{ID}}", id)
			reset := ""
			if tx.Dialector.Name() == "postgres" {
				reset = "SELECT setval(pg_get_serial_sequence('queue_jobs','id'), COALESCE((SELECT MAX(id) FROM queue_jobs),1), EXISTS(SELECT 1 FROM queue_jobs))"
			}
			ddl = strings.ReplaceAll(ddl, "{{SEQUENCE_RESET}}", reset)
			cancelDDL := `CREATE TABLE queue_jobs_v37 (id ` + id + `,agent_id BIGINT NOT NULL DEFAULT 0,bot_id BIGINT NOT NULL DEFAULT 0,chat_id BIGINT NOT NULL,thread_id BIGINT NOT NULL DEFAULT 0,sender_chat_id BIGINT NOT NULL DEFAULT 0,actor_user_id BIGINT NOT NULL DEFAULT 0 CHECK(actor_user_id>=0),dedupe_key TEXT NOT NULL DEFAULT '',kind TEXT NOT NULL,payload TEXT NOT NULL DEFAULT '{}',status TEXT NOT NULL CHECK(status IN('pending','leased','retry','completed','dead','cancelled')),attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts>=0),max_attempts INTEGER NOT NULL CHECK(max_attempts BETWEEN 1 AND 100),available_at BIGINT NOT NULL DEFAULT 0,lease_until BIGINT NOT NULL DEFAULT 0,lease_token TEXT NOT NULL DEFAULT '',failure_class TEXT NOT NULL DEFAULT '',priority SMALLINT NOT NULL DEFAULT 0 CHECK(priority BETWEEN 0 AND 9),created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL,UNIQUE(agent_id,bot_id,chat_id,dedupe_key));INSERT INTO queue_jobs_v37 SELECT id,agent_id,bot_id,chat_id,thread_id,sender_chat_id,actor_user_id,dedupe_key,kind,payload,status,attempts,max_attempts,available_at,lease_until,lease_token,failure_class,priority,created_at,updated_at FROM queue_jobs;DROP TABLE queue_jobs;ALTER TABLE queue_jobs_v37 RENAME TO queue_jobs;CREATE INDEX queue_ready ON queue_jobs(status,available_at,id);CREATE INDEX queue_expired ON queue_jobs(status,lease_until,id);CREATE INDEX queue_scope_active ON queue_jobs(agent_id,bot_id,chat_id,id) WHERE status IN('pending','leased','retry');CREATE INDEX queue_actor ON queue_jobs(agent_id,actor_user_id,status);CREATE INDEX queue_scope_priority ON queue_jobs(agent_id,bot_id,chat_id,priority,id) WHERE status IN('pending','leased','retry')`
			if tx.Dialector.Name() == "postgres" {
				cancelDDL = `ALTER TABLE queue_jobs DROP CONSTRAINT IF EXISTS queue_jobs_status_check;ALTER TABLE queue_jobs ADD CONSTRAINT queue_jobs_status_check CHECK(status IN('pending','leased','retry','completed','dead','cancelled'))`
			}
			ddl = strings.ReplaceAll(ddl, "{{STATUS_CANCEL_DDL}}", cancelDDL)
			skillPendingDDL := `CREATE TABLE agent_skills_v40 (id ` + id + `,agent_id TEXT NOT NULL,bot_id BIGINT NOT NULL,name TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',version BIGINT NOT NULL,source TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN('draft','active','failed','pending')),test_input TEXT NOT NULL DEFAULT '{}',test_output TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created_by BIGINT NOT NULL,created_at BIGINT NOT NULL,updated_at BIGINT NOT NULL,UNIQUE(agent_id,bot_id,name,version));INSERT INTO agent_skills_v40 SELECT id,agent_id,bot_id,name,description,version,source,status,test_input,test_output,error,created_by,created_at,updated_at FROM agent_skills;DROP TABLE agent_skills;ALTER TABLE agent_skills_v40 RENAME TO agent_skills;CREATE INDEX agent_skills_catalog ON agent_skills(agent_id,bot_id,name,version DESC)`
			if tx.Dialector.Name() == "postgres" {
				skillPendingDDL = `ALTER TABLE agent_skills DROP CONSTRAINT IF EXISTS agent_skills_status_check;ALTER TABLE agent_skills ADD CONSTRAINT agent_skills_status_check CHECK(status IN('draft','active','failed','pending'))`
			}
			ddl = strings.ReplaceAll(ddl, "{{SKILL_PENDING_DDL}}", skillPendingDDL)
			dropV3StatusCheckDDL := ""
			if tx.Dialector.Name() == "postgres" {
				dropV3StatusCheckDDL = "ALTER TABLE queue_jobs DROP CONSTRAINT IF EXISTS queue_jobs_v3_status_check"
			}
			ddl = strings.ReplaceAll(ddl, "{{DROP_V3_STATUS_CHECK_DDL}}", dropV3StatusCheckDDL)
			for _, statement := range strings.Split(ddl, ";") {
				if strings.TrimSpace(statement) == "" {
					continue
				}
				if err := tx.Exec(statement).Error; err != nil {
					return err
				}
			}
			if err := tx.Exec("INSERT INTO schema_migrations (version) VALUES (?)", version).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return errors.New("database migration failed")
	}
	return nil
}
