package storage

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/maniack/tgaia/internal/access"
	"gorm.io/gorm"
)

type BotRegistry struct{ db *gorm.DB }

func (d *Database) Bots() *BotRegistry { return &BotRegistry{db: d.orm} }
func validAgent(id string) bool        { return strings.TrimSpace(id) != "" && len(id) <= 128 }

// Register associates one Telegram bot identity with one agent. CredentialRef
// names a secret in runtime configuration; the token itself is never stored here.
// Reassignment or secret rotation are separate administrative operations.
func (b *BotRegistry) Register(ctx context.Context, agentID string, botID int64, credentialRef string) (bool, error) {
	if !validAgent(agentID) || botID <= 0 || len(credentialRef) > 128 || !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(credentialRef) {
		return false, errors.New("invalid bot registration")
	}
	applied := false
	err := b.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO agents (agent_id) VALUES (?) ON CONFLICT (agent_id) DO NOTHING", agentID).Error; err != nil {
			return err
		}
		result := tx.Exec("INSERT INTO telegram_bots (agent_id,bot_id,credential_ref) VALUES (?,?,?) ON CONFLICT DO NOTHING", agentID, botID, credentialRef)
		if result.Error != nil {
			return result.Error
		}
		applied = result.RowsAffected == 1
		if !applied {
			var row struct {
				AgentID       string
				CredentialRef string
			}
			if err := tx.Table("telegram_bots").Where("bot_id=?", botID).Take(&row).Error; err != nil {
				return err
			}
			if row.AgentID != agentID || row.CredentialRef != credentialRef {
				return errors.New("conflicting bot registration")
			}
		}
		return nil
	})
	if err != nil {
		return false, errors.New("bot registration failed or conflicts with existing owner")
	}
	return applied, nil
}
func (b *BotRegistry) CredentialRef(ctx context.Context, agentID string, botID int64) (string, error) {
	if !validAgent(agentID) || botID <= 0 {
		return "", errors.New("invalid bot identity")
	}
	var row struct{ CredentialRef string }
	if err := b.db.WithContext(ctx).Table("telegram_bots").Where("agent_id=? AND bot_id=?", agentID, botID).Take(&row).Error; err != nil {
		return "", errors.New("bot not available for this agent")
	}
	return row.CredentialRef, nil
}

type Access struct{ db *gorm.DB }

func (d *Database) Access() *Access { return &Access{db: d.orm} }

// SetUserBlocked is an administrative persistence operation, not an endpoint.
// The caller must authenticate/authorize the administrator before calling it.
func (a *Access) SetUserBlocked(ctx context.Context, agentID string, userID int64, blocked bool) (bool, error) {
	if !validAgent(agentID) || userID <= 0 {
		return false, errors.New("invalid user block")
	}
	query := "DELETE FROM blocked_users WHERE agent_id=? AND user_id=?"
	if blocked {
		query = "INSERT INTO blocked_users (agent_id,user_id) VALUES (?,?) ON CONFLICT (agent_id,user_id) DO NOTHING"
	}
	changed := false
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			var id int64
			if err := tx.Raw("SELECT user_id FROM seen_users WHERE agent_id=? AND user_id=? FOR UPDATE", agentID, userID).Scan(&id).Error; err != nil {
				return err
			}
		}
		result := tx.Exec(query, agentID, userID)
		if result.Error != nil {
			return result.Error
		}
		changed = result.RowsAffected == 1
		if blocked {
			return retireUser(tx, agentID, userID)
		}
		return nil
	})
	return changed, err
}

// CheckUser has no chat or bot parameter: local grants cannot bypass the block.
// Unknown/anonymous authors need an explicit application policy, not userID=0.
func (a *Access) CheckUser(ctx context.Context, agentID string, userID int64) error {
	if !validAgent(agentID) || userID <= 0 {
		return errors.New("invalid user identity")
	}
	var count int64
	if err := a.db.WithContext(ctx).Table("blocked_users").Where("agent_id=? AND user_id=?", agentID, userID).Count(&count).Error; err != nil {
		return errors.New("checking user access failed")
	}
	if count > 0 {
		return access.ErrBlocked
	}
	return nil
}
