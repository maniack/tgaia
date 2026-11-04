package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSecretRedaction(t *testing.T) {
	secret := NewSecret("sensitive-value")
	cfg := Config{Database: Database{DSN: secret}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config", "dsn", secret, "config", cfg)
	output := fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg) + string(data) + logs.String()
	if strings.Contains(output, "sensitive-value") {
		t.Fatal("secret leaked")
	}
	if secret.Reveal() != "sensitive-value" {
		t.Fatal("secret inaccessible to adapter")
	}
}

func TestOIDCAdminGroupsConfiguration(t *testing.T) {
	cfg := App{
		AgentID: "test", Listen: "0.0.0.0:8080", PublicURL: "https://example.invalid", Role: "api",
		LLMEndpoint: "https://llm.example.invalid", LLMModel: "test", OIDCMode: "oidc",
		OIDCIssuer: "https://auth.example.invalid/realm", OIDCClientID: "test",
		SessionKey: NewSecret(strings.Repeat("s", 32)), TelegramMode: "polling",
		Bots: []Bot{{Name: "bot", Token: NewSecret("1:test")}}, MemoryWindow: time.Hour, MemoryInterval: time.Minute,
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "admin groups") {
		t.Fatalf("missing admin groups accepted: %v", err)
	}
	cfg.AdminGroups = []string{"/team/admins"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("admin groups rejected: %v", err)
	}
	groups := splitCSV(" /team/admins, platform-admins, ,/team/admins ")
	if strings.Join(groups, "|") != "/team/admins|platform-admins|/team/admins" {
		t.Fatalf("unexpected groups: %#v", groups)
	}
	if err := ValidateEnv([]string{"TGAIA_ADMIN_GROUPS=/team/admins"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnv([]string{"TGAIA_ADMIN_SUBJECTS=legacy"}); err == nil {
		t.Fatal("legacy subject allowlist environment variable accepted")
	}
}
