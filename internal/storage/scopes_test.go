package storage

import (
	"context"
	"github.com/maniack/tgaia/internal/config"
	"strings"
	"testing"
)

func TestScopedSettingsPrecedenceAndSecrets(t *testing.T) {
	db, _ := queueDatabases(t, config.SQLite)
	ctx := context.Background()
	settings, err := db.Settings(config.NewSecret(strings.Repeat("x", 40)))
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := settings.InScope("worker")
	api, _ := settings.InScope("api")
	for _, entry := range []struct {
		s          *Settings
		key, value string
	}{{settings, "llm.model", "shared"}, {worker, "llm.model", "worker-model"}, {settings, "llm.api_key", "shared-secret"}, {worker, "llm.api_key", "worker-secret"}, {api, "limits.telegram_timeout", "40s"}} {
		if err := entry.s.Put(ctx, entry.key, entry.value, 0); err != nil {
			t.Fatal(err)
		}
	}
	w, _, err := settings.LoadResolved(ctx, "worker")
	if err != nil || w["llm.model"] != "worker-model" || w["llm.api_key"] != "worker-secret" {
		t.Fatal("worker precedence", err)
	}
	a, _, err := settings.LoadResolved(ctx, "api")
	if err != nil || a["llm.model"] != "shared" || a["limits.telegram_timeout"] != "40s" || a["llm.api_key"] != "shared-secret" {
		t.Fatal("API isolation", err)
	}
	if err := api.Put(ctx, "llm.model", "wrong", 0); err == nil {
		t.Fatal("wrong scope accepted")
	}
	if err := worker.Put(ctx, "role", "api", 0); err == nil {
		t.Fatal("role override accepted")
	}
	if err := settings.Put(ctx, "listen", "127.0.0.1:1", 0); err == nil {
		t.Fatal("deployment-local listener accepted in shared scope")
	}
	if err := worker.Put(ctx, "public_url", "http://worker.invalid", 0); err == nil {
		t.Fatal("worker public URL accepted")
	}
	if err := api.Put(ctx, "listen", "127.0.0.1:2", 0); err != nil {
		t.Fatal("API listener rejected", err)
	}
	if err := worker.Put(ctx, "listen", "127.0.0.1:3", 0); err == nil {
		t.Fatal("unused worker application listener accepted")
	}
	apiValues, _, err := settings.LoadResolved(ctx, "api")
	if err != nil || apiValues["listen"] != "127.0.0.1:2" {
		t.Fatal("API listener scope not resolved", err)
	}
	if err := worker.Delete(ctx, "llm.model", 1); err != nil {
		t.Fatal(err)
	}
	w, _, err = settings.LoadResolved(ctx, "worker")
	if err != nil || w["llm.model"] != "shared" {
		t.Fatal("reset lost inheritance", err)
	}
	// A ciphertext copied across scopes cannot be decrypted under the other namespace.
	if err := db.orm.Exec("UPDATE runtime_settings SET value=(SELECT value FROM runtime_settings WHERE scope='worker' AND name='llm.api_key') WHERE scope='shared' AND name='llm.api_key'").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := settings.Load(ctx); err == nil {
		t.Fatal("cross-scope ciphertext accepted")
	}
}
func TestLegacySettingsMigration(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 15)
	ctx := context.Background()
	if err := db.orm.Exec("INSERT INTO runtime_settings(name,value,secret) VALUES ('llm.model','legacy',FALSE),('role','worker',FALSE),('listen','127.0.0.1:9999',FALSE),('public_url','http://legacy.invalid',FALSE)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	settings, _ := db.Settings(config.NewSecret(strings.Repeat("k", 40)))
	v, _, err := settings.LoadResolved(ctx, "api")
	if err != nil || v["llm.model"] != "legacy" {
		t.Fatal("legacy values lost", err)
	}
	if _, ok := v["role"]; ok {
		t.Fatal("legacy role affected replica")
	}
	for _, name := range []string{"role", "listen", "public_url"} {
		var count int64
		if err := db.orm.Table("runtime_settings").Where("name=?", name).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("legacy deployment-local setting %s survived migration", name)
		}
	}
}
