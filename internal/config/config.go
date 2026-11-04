// Package config owns the validated runtime configuration and secret handling.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

type Driver string

const (
	SQLite   Driver = "sqlite"
	Postgres Driver = "postgres"
)

// Secret prevents accidental disclosure through common diagnostic encoders.
type Secret struct{ value string }

func NewSecret(value string) Secret         { return Secret{value: value} }
func (s Secret) Reveal() string             { return s.value }
func (Secret) String() string               { return "[REDACTED]" }
func (Secret) GoString() string             { return "[REDACTED]" }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[REDACTED]") }

type Database struct {
	Driver Driver `json:"driver"`
	DSN    Secret `json:"dsn"`
}
type Config struct {
	Database       Database      `json:"database"`
	LogLevel       slog.Level    `json:"log_level"`
	ConnectTimeout time.Duration `json:"connect_timeout"`
}

func (c Config) Validate() error {
	if c.ConnectTimeout <= 0 || c.ConnectTimeout > time.Minute {
		return errors.New("connect-timeout must be greater than zero and at most 1m")
	}
	if strings.TrimSpace(c.Database.DSN.Reveal()) == "" {
		return errors.New("database DSN is required (config file or TGAIA_DATABASE_DSN)")
	}
	switch c.Database.Driver {
	case SQLite:
	case Postgres:
		u, err := url.Parse(c.Database.DSN.Reveal())
		if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || strings.Trim(u.Path, "/") == "" {
			return errors.New("postgres DSN must be a postgres:// or postgresql:// URL with host and database")
		}
	default:
		return fmt.Errorf("database driver must be sqlite or postgres")
	}
	return nil
}
