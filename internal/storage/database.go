// Package storage isolates GORM and SQL driver details from the application.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/maniack/tgaia/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Database struct {
	orm *gorm.DB
	sql *sql.DB
}

// Open connects without changing the schema. The caller owns Close.
func Open(ctx context.Context, cfg config.Config) (*Database, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var dialect gorm.Dialector
	switch cfg.Database.Driver {
	case config.SQLite:
		dsn := cfg.Database.DSN.Reveal()
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		// Driver parameters apply to every new connection, including replacements.
		dialect = sqlite.Open(dsn + separator + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	case config.Postgres:
		dialect = postgres.Open(cfg.Database.DSN.Reveal())
	}
	db, err := gorm.Open(dialect, &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               logger.Default.LogMode(logger.Silent),
		NowFunc:              func() time.Time { return time.Now().UTC() },
	})
	// Driver errors may contain passwords or the DSN. Never return them raw.
	if err != nil {
		return nil, errors.New("database initialization failed")
	}
	pool, err := db.DB()
	if err != nil {
		return nil, errors.New("database pool initialization failed")
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		if pingCtx.Err() != nil {
			return nil, pingCtx.Err()
		}
		return nil, errors.New("database connection failed; check DSN and server availability")
	}
	return &Database{orm: db, sql: pool}, nil
}
func (d *Database) Close() error {
	if err := d.sql.Close(); err != nil {
		return errors.New("database close failed")
	}
	return nil
}
