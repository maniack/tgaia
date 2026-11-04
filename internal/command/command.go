// Package command is the composition root for the CLI and concrete adapters.
package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/app"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/urfave/cli/v3"
)

func New(out, errOut io.Writer) *cli.Command {
	file := &config.File{}
	var cfg config.Config
	loadApp := func() (config.App, error) {
		appConfig, err := file.App()
		if err != nil {
			return config.App{}, err
		}
		appConfig.LogLevel = cfg.LogLevel
		return appConfig, nil
	}
	migrate := func(ctx context.Context) error {
		db, err := storage.Open(ctx, cfg)
		if err != nil {
			return err
		}
		migrateErr := db.Migrate(ctx)
		closeErr := db.Close()
		if migrateErr != nil {
			return migrateErr
		}
		return closeErr
	}
	serve := func(ctx context.Context, appConfig config.App) error {
		return app.Run(ctx, cfg, appConfig, out)
	}
	return &cli.Command{
		Name: "tgaia", Usage: "Telegram AI agent; without a command, migrate then serve",
		Writer: out, ErrWriter: errOut, HideVersion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "config", Usage: "local YAML configuration file", Destination: &file.Path, Sources: cli.EnvVars("TGAIA_CONFIG")},
			&cli.StringFlag{Name: "database-driver", Value: "sqlite", Sources: file.Source("database.driver", "TGAIA_DATABASE_DRIVER")},
			&cli.StringFlag{Name: "log-level", Value: "info", Sources: file.Source("log_level", "TGAIA_LOG_LEVEL")},
			&cli.DurationFlag{Name: "connect-timeout", Value: 5 * time.Second, Sources: file.Source("connect_timeout", "TGAIA_CONNECT_TIMEOUT")},
		},
		Before: func(ctx context.Context, cmd *cli.Command) (next context.Context, result error) {
			defer func() {
				if result != nil {
					result = safeError{result}
				}
			}()
			if err := config.ValidateEnv(os.Environ()); err != nil {
				return ctx, err
			}
			if err := file.Load(); err != nil {
				return ctx, err
			}
			cfg.Database.Driver = config.Driver(cmd.String("database-driver"))
			dsn, _ := file.Value("database.dsn")
			if value, ok := os.LookupEnv("TGAIA_DATABASE_DSN"); ok {
				dsn = value
			}
			cfg.Database.DSN = config.NewSecret(dsn)
			cfg.ConnectTimeout = cmd.Duration("connect-timeout")
			level := strings.ToLower(strings.TrimSpace(cmd.String("log-level")))
			switch level {
			case "debug", "info", "warn", "error":
			default:
				return ctx, errors.New("log-level must be debug, info, warn or error")
			}
			if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
				return ctx, errors.New("log-level must be debug, info, warn or error")
			}
			return ctx, cfg.Validate()
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() != 0 {
				return safeError{errors.New("unknown command (see --help)")}
			}
			appConfig, err := loadApp()
			if err != nil {
				return safeError{err}
			}
			if err = migrate(ctx); err != nil {
				return safeError{err}
			}
			if err = serve(ctx, appConfig); err != nil {
				return safeError{err}
			}
			return nil
		},
		Commands: []*cli.Command{
			{Name: "serve", Usage: "run without migrations (for production after db-migrate)", Action: func(ctx context.Context, cmd *cli.Command) error {
				if cmd.Args().Len() != 0 {
					return safeError{errors.New("serve does not accept arguments")}
				}
				appConfig, err := loadApp()
				if err != nil {
					return safeError{err}
				}
				if err := serve(ctx, appConfig); err != nil {
					return safeError{err}
				}
				return nil
			}},
			{Name: "db-migrate", Usage: "apply versioned database migrations explicitly", Action: func(ctx context.Context, cmd *cli.Command) error {
				if cmd.Args().Len() != 0 {
					return safeError{errors.New("db-migrate does not accept arguments")}
				}
				if err := migrate(ctx); err != nil {
					return safeError{err}
				}
				return nil
			}},
			{Name: "config-check", Usage: "validate configuration and print a redacted snapshot", Action: func(ctx context.Context, cmd *cli.Command) error {
				if cmd.Args().Len() != 0 {
					return errors.New("config-check does not accept arguments")
				}
				return json.NewEncoder(out).Encode(cfg)
			}},
			{Name: "db-check", Usage: "check database connectivity without migrating the schema", Action: func(ctx context.Context, cmd *cli.Command) error {
				if cmd.Args().Len() != 0 {
					return errors.New("db-check does not accept arguments")
				}
				db, err := storage.Open(ctx, cfg)
				if err != nil {
					return safeError{err}
				}
				if err := db.Close(); err != nil {
					return safeError{err}
				}
				slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: cfg.LogLevel})).InfoContext(ctx, "database connection verified", "driver", cfg.Database.Driver)
				return nil
			}},
		},
	}
}

// Run deliberately sanitizes parser errors too: third-party flag parsing can
// echo arbitrary input, including a secret accidentally supplied as a flag.
func Run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if err := New(out, io.Discard).Run(ctx, args); err != nil {
		var safe safeError
		if errors.As(err, &safe) {
			_, _ = fmt.Fprintln(errOut, "tgaia:", safe.Error())
			return safe
		}
		_, _ = io.WriteString(errOut, "tgaia: invalid command or flag value (see --help)\n")
		return errors.New("invalid command or flag value")
	}
	return nil
}

// safeError marks diagnostics that contain no untrusted input.
type safeError struct{ error }
