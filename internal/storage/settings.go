package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/maniack/tgaia/internal/config"
	"gorm.io/gorm"
)

type Settings struct {
	scope  string
	db     *gorm.DB
	cipher cipher.AEAD
}
type Setting struct {
	Scope   string `json:"scope"`
	Name    string `json:"name"`
	Value   string `json:"value"`
	Secret  bool   `json:"secret"`
	Version int64  `json:"version"`
}

func (d *Database) Settings(key config.Secret) (*Settings, error) {
	if len(key.Reveal()) < 32 {
		return nil, errors.New("bootstrap session key must have at least 32 characters")
	}
	derived := sha256.Sum256([]byte("tgaia/settings/v1\x00" + key.Reveal()))
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Settings{db: d.orm, cipher: aead}, nil
}
func (s *Settings) Load(ctx context.Context) (map[string]string, []Setting, error) {
	var rows []Setting
	if err := s.db.WithContext(ctx).Table("runtime_settings").Where("scope=?", s.Scope()).Order("name").Find(&rows).Error; err != nil {
		return nil, nil, errors.New("reading settings failed")
	}
	values := map[string]string{}
	for i, row := range rows {
		value := row.Value
		if row.Secret {
			data, err := base64.RawStdEncoding.DecodeString(value)
			if err != nil || len(data) < s.cipher.NonceSize() {
				return nil, nil, errors.New("invalid encrypted setting")
			}
			plain, err := s.cipher.Open(nil, data[:s.cipher.NonceSize()], data[s.cipher.NonceSize():], s.aad(row.Name))
			if err != nil {
				return nil, nil, errors.New("cannot decrypt settings with bootstrap key")
			}
			value = string(plain)
			rows[i].Value = ""
		}
		values[row.Name] = value
	}
	return values, rows, nil
}
func (s *Settings) Put(ctx context.Context, name, value string, version int64) error {
	if !config.AllowedSettingScope(s.Scope(), name) || name == "" || strings.HasPrefix(name, "oidc.") || strings.HasPrefix(name, "database.") || name == "session_key" || len(value) > 32000 {
		return errors.New("invalid or bootstrap-only setting")
	}
	secret := config.SecretSetting(name)
	if secret {
		nonce := make([]byte, s.cipher.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return errors.New("cannot encrypt setting")
		}
		value = base64.RawStdEncoding.EncodeToString(s.cipher.Seal(nonce, nonce, []byte(value), s.aad(name)))
	}
	var result *gorm.DB
	if version == 0 {
		result = s.db.WithContext(ctx).Exec("INSERT INTO runtime_settings (scope,name,value,secret) VALUES (?,?,?,?) ON CONFLICT DO NOTHING", s.Scope(), name, value, secret)
	} else {
		result = s.db.WithContext(ctx).Exec("UPDATE runtime_settings SET value=?,secret=?,version=version+1 WHERE scope=? AND name=? AND version=?", value, secret, s.Scope(), name, version)
	}
	if result.Error != nil || result.RowsAffected != 1 {
		return errors.New("settings version conflict")
	}
	return nil
}
func (s *Settings) Delete(ctx context.Context, name string, version int64) error {
	r := s.db.WithContext(ctx).Exec("DELETE FROM runtime_settings WHERE scope=? AND name=? AND version=?", s.Scope(), name, version)
	if r.Error != nil || r.RowsAffected != 1 {
		return errors.New("settings version conflict")
	}
	return nil
}
func (b *BotRegistry) Prompt(ctx context.Context, agentID string, botID int64) (string, error) {
	var row struct{ SystemPrompt string }
	if err := b.db.WithContext(ctx).Table("telegram_bots").Where("agent_id=? AND bot_id=?", agentID, botID).Take(&row).Error; err != nil {
		return "", errors.New("bot prompt unavailable")
	}
	return row.SystemPrompt, nil
}
func (b *BotRegistry) SetPrompt(ctx context.Context, agentID string, botID int64, prompt string) error {
	if !validAgent(agentID) || botID <= 0 || len(prompt) > 16000 {
		return errors.New("invalid bot prompt")
	}
	r := b.db.WithContext(ctx).Table("telegram_bots").Where("agent_id=? AND bot_id=?", agentID, botID).Update("system_prompt", prompt)
	if r.Error != nil || r.RowsAffected != 1 {
		return errors.New("updating bot prompt failed")
	}
	return nil
}

func (s *Settings) Scope() string {
	if s.scope == "" {
		return "shared"
	}
	return s.scope
}
func (s *Settings) InScope(scope string) (*Settings, error) {
	if scope == "" {
		scope = "shared"
	}
	if scope != "shared" && scope != "api" && scope != "worker" {
		return nil, errors.New("invalid settings scope")
	}
	copy := *s
	copy.scope = scope
	return &copy, nil
}
func (s *Settings) aad(name string) []byte {
	if s.Scope() == "shared" {
		return []byte(name)
	}
	return []byte(s.Scope() + "\x00" + name)
}
func (s *Settings) LoadResolved(ctx context.Context, role string) (map[string]string, []Setting, error) {
	shared, _ := s.InScope("shared")
	values, rows, err := shared.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Legacy or manually inserted deployment-local values in shared scope must
	// never change every replica. Migration 016 drops them, and this remains a
	// defense for databases edited outside the application.
	delete(values, "role")
	delete(values, "listen")
	delete(values, "public_url")
	if role == "all" {
		role = "worker"
	}
	if role == "api" || role == "worker" {
		special, _ := s.InScope(role)
		overrides, extra, err := special.Load(ctx)
		if err != nil {
			return nil, nil, err
		}
		for name, v := range overrides {
			if !config.AllowedSettingScope(role, name) {
				return nil, nil, errors.New("setting is not allowed in replica scope")
			}
			values[name] = v
		}
		rows = append(rows, extra...)
	}
	return values, rows, nil
}
