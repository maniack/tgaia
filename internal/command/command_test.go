package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func configFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigurationPrecedence(t *testing.T) {
	path := configFile(t, "database:\n  driver: sqlite\n  dsn: ':memory:'\nlog_level: warn\nconnect_timeout: 3s\n")
	for _, tc := range []struct{ name, env, flag, want string }{
		{"file", "", "", "WARN"},
		{"environment", "error", "", "ERROR"},
		{"flag", "error", "debug", "DEBUG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("TGAIA_LOG_LEVEL", tc.env)
			}
			args := []string{"tgaia", "--config", path}
			if tc.flag != "" {
				args = append(args, "--log-level", tc.flag)
			}
			args = append(args, "config-check")
			var out bytes.Buffer
			if err := New(&out, &out).Run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			var got struct {
				LogLevel string `json:"log_level"`
			}
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.LogLevel != tc.want {
				t.Fatalf("got %s want %s", got.LogLevel, tc.want)
			}
			if strings.Contains(out.String(), ":memory:") {
				t.Fatal("DSN leaked")
			}
		})
	}
}

func TestConfigPathFromEnvironmentAndFlagOverride(t *testing.T) {
	t.Setenv("TGAIA_CONFIG", configFile(t, "database:\n  dsn: ':memory:'\nlog_level: error\n"))
	for _, args := range [][]string{
		{"tgaia", "config-check"},
		{"tgaia", "--config", configFile(t, "database:\n  dsn: ':memory:'\n"), "config-check"},
	} {
		var out bytes.Buffer
		if err := New(&out, &out).Run(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"unknown key", "database:\n  dsn: ':memory:'\nlog_levle: info\n"},
		{"nested unknown", "database:\n  dsn: ':memory:'\n  password: secret\n"},
		{"duplicate", "database:\n  dsn: first\n  dsn: second\n"},
		{"multiple documents", "database:\n  dsn: ':memory:'\n---\n{}\n"},
		{"zero timeout", "database:\n  dsn: ':memory:'\nconnect_timeout: 0s\n"},
		{"driver", "database:\n  driver: mysql\n  dsn: ':memory:'\n"},
		{"no dsn", "{}"},
		{"numeric scalar", "database:\n  dsn: 123\n"},
		{"null scalar", "database:\n  dsn: null\n"},
		{"bad postgres", "database:\n  driver: postgres\n  dsn: 'postgres://user:password@/'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := New(&out, &out).Run(context.Background(), []string{"tgaia", "--config", configFile(t, tc.body), "config-check"})
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestUnknownEnvironment(t *testing.T) {
	t.Setenv("TGAIA_DATABASE_DSN", ":memory:")
	t.Setenv("TGAIA_LOG_LEVLE", "debug")
	var out bytes.Buffer
	if err := New(&out, &out).Run(context.Background(), []string{"tgaia", "config-check"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestMissingFileEvenWithAllOverrides(t *testing.T) {
	t.Setenv("TGAIA_DATABASE_DSN", ":memory:")
	var out bytes.Buffer
	err := New(&out, &out).Run(context.Background(), []string{"tgaia", "--config", filepath.Join(t.TempDir(), "missing"), "--log-level", "info", "--database-driver", "sqlite", "--connect-timeout", "1s", "config-check"})
	if err == nil {
		t.Fatal("missing file was ignored")
	}
}

func TestPublicErrorsDoNotEchoSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"tgaia", "--database-dsn", "sensitive-value", "config-check"},
		{"tgaia", "--connect-timeout", "sensitive-value", "config-check"},
		{"tgaia", "--config", configFile(t, "database:\n  dsn: sensitive-value\nlog_level: sensitive-value\n"), "config-check"},
	} {
		var out bytes.Buffer
		err := Run(context.Background(), args, &out, &out)
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(out.String()+err.Error(), "sensitive-value") {
			t.Fatal("secret leaked")
		}
	}
}

func TestHelpWithoutConfiguration(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"tgaia", "--help"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "config-check") {
		t.Fatal("missing help")
	}
}

func testAppConfig(t *testing.T, database, address, observabilityAddress string) string {
	t.Helper()
	return configFile(t, fmt.Sprintf(`database:
  driver: sqlite
  dsn: %q
app:
  agent_id: test
  listen: %s
  public_url: http://%s
  role: api
  observability:
    listen: %s
    otlp_endpoint: ""
  llm:
    endpoint: ""
    model: ""
    api_key: ""
    mock: "true"
  oidc:
    mode: mock
    issuer: ""
    client_id: test
    client_secret: ""
    session_key: "01234567890123456789012345678901"
    admin_groups: local-admins
  telegram:
    mode: mock
    bots:
      test:
        id: "1"
        username: test_bot
        token: ""
        webhook_secret: ""
  memory:
    window: 1h
    interval: 30s
`, database, address, address, observabilityAddress))
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func TestDefaultCommandMigratesThenServes(t *testing.T) {
	address := freeAddress(t)
	observabilityAddress := freeAddress(t)
	path := testAppConfig(t, filepath.Join(t.TempDir(), "default.db"), address, observabilityAddress)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- New(io.Discard, io.Discard).Run(ctx, []string{"tgaia", "--config", path})
	}()

	client := &http.Client{Timeout: 250 * time.Millisecond}
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		response, err := client.Get("http://" + observabilityAddress + "/readyz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		cancel()
		<-done
		t.Fatal("default command did not migrate and become ready")
	}
	response, err := client.Get("http://" + address + "/healthz")
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusOK {
		cancel()
		<-done
		t.Fatal("health probe exposed on application listener")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServeDoesNotMigrate(t *testing.T) {
	address := freeAddress(t)
	path := testAppConfig(t, filepath.Join(t.TempDir(), "unmigrated.db"), address, freeAddress(t))
	err := New(io.Discard, io.Discard).Run(context.Background(), []string{"tgaia", "--config", path, "serve"})
	if err == nil {
		t.Fatal("explicit serve migrated an empty database")
	}
}
