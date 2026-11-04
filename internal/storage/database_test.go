package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
)

// Both drivers run the same connectivity/transaction contract. A real PostgreSQL
// instance is required; SQLite is not a substitute for production SQL semantics.
func TestDatabaseContract(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "test.db")
			if driver == config.Postgres {
				dsn = os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("set TEST_POSTGRES_DSN to run against PostgreSQL")
				}
			}
			cfg := config.Config{Database: config.Database{Driver: driver, DSN: config.NewSecret(dsn)}, ConnectTimeout: 5 * time.Second}
			db, err := Open(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tx := db.orm.Begin()
			if tx.Error != nil {
				t.Fatal("begin transaction failed")
			}
			defer tx.Rollback()
			if err := tx.Exec("CREATE TEMPORARY TABLE tgaia_contract (id BIGINT PRIMARY KEY, value TEXT NOT NULL)").Error; err != nil {
				t.Fatal("create table failed")
			}
			if err := tx.Exec("INSERT INTO tgaia_contract (id, value) VALUES (?, ?)", 1, "ok").Error; err != nil {
				t.Fatal("insert failed")
			}
			var value string
			if err := tx.Raw("SELECT value FROM tgaia_contract WHERE id = ?", 1).Scan(&value).Error; err != nil || value != "ok" {
				t.Fatal("read failed")
			}
			if driver == config.SQLite {
				var enabled int
				if err := tx.Raw("PRAGMA foreign_keys").Scan(&enabled).Error; err != nil || enabled != 1 {
					t.Fatal("foreign keys disabled")
				}
			}
		})
	}
}

func TestCancelledOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(":memory:")}, ConnectTimeout: time.Second}
	db, err := Open(ctx, cfg)
	if err == nil {
		db.Close()
		t.Fatal("expected cancelled connection")
	}
}
