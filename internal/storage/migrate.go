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
func (d *Database) Migrate(ctx context.Context) error { return d.migrate(ctx, 26) }

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
		for index, path := range []string{"migrations/001_queue.sql", "migrations/002_bot_messages.sql", "migrations/003_bots_and_access.sql", "migrations/004_poc.sql", "migrations/005_settings.sql", "migrations/006_polling.sql", "migrations/007_runtime_identity.sql", "migrations/008_memory_revision.sql", "migrations/009_mcp.sql", "migrations/010_admin.sql", "migrations/011_telegram.sql", "migrations/012_reply_links.sql", "migrations/013_roles.sql", "migrations/014_audit.sql", "migrations/015_quotas.sql", "migrations/016_setting_scopes.sql", "migrations/017_explicit_memory.sql", "migrations/018_chat_memory.sql", "migrations/019_reply_to.sql", "migrations/020_forum_state.sql", "migrations/021_user_telegram_profile.sql", "migrations/022_skills_vfs.sql", "migrations/023_message_reactions.sql", "migrations/024_bot_behavior.sql", "migrations/025_failure_class.sql", "migrations/026_chat_tool_limits.sql"} {
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
