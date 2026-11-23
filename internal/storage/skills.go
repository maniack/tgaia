package storage

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var skillName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

type Skill struct {
	ID          int64  `json:"id"`
	AgentID     string `json:"agent_id"`
	BotID       int64  `json:"bot_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     int64  `json:"version"`
	Source      string `json:"source"`
	Status      string `json:"status"`
	TestInput   string `json:"test_input"`
	TestOutput  string `json:"test_output"`
	Error       string `json:"error"`
	CreatedBy   int64  `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type VirtualFile struct {
	ID        int64  `json:"id"`
	AgentID   string `json:"agent_id"`
	BotID     int64  `json:"bot_id"`
	ChatID    int64  `json:"chat_id"`
	UserID    int64  `json:"user_id"`
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
	Content   string `json:"content"`
	MIMEType  string `json:"mime_type"`
	Version   int64  `json:"version"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type VirtualFileChange struct {
	Path    string
	Content *string
}

func (a *AgentStore) SaveSkill(ctx context.Context, agentID string, botID, userID int64, name, description, source string, requiresApproval bool) (Skill, error) {
	var saved Skill
	name, description, source = strings.TrimSpace(name), strings.TrimSpace(description), strings.TrimSpace(source)
	if agentID == "" || botID <= 0 || userID <= 0 || !skillName.MatchString(name) || description == "" || len(description) > 500 || source == "" || len(source) > 64000 {
		return saved, errors.New("invalid skill")
	}
	initialStatus := "draft"
	if requiresApproval {
		initialStatus = "pending"
	}
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest Skill
		err := tx.Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=?", agentID, botID, name).Order("version DESC").Clauses(clause.Locking{Strength: "UPDATE"}).Take(&latest).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		version := latest.Version + 1
		now := time.Now().UTC().UnixMilli()
		row := map[string]any{"agent_id": agentID, "bot_id": botID, "name": name, "description": description, "version": version, "source": source, "status": initialStatus, "created_by": userID, "created_at": now, "updated_at": now}
		if err := tx.Table("agent_skills").Create(row).Error; err != nil {
			return err
		}
		return tx.Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=? AND version=?", agentID, botID, name, version).Take(&saved).Error
	})
	if err != nil {
		return Skill{}, errors.New("saving skill failed")
	}
	return saved, nil
}

func (a *AgentStore) Skills(ctx context.Context, agentID string, botID int64) ([]Skill, error) {
	var rows []Skill
	if agentID == "" || botID <= 0 {
		return rows, errors.New("invalid skill scope")
	}
	q := `SELECT s.* FROM agent_skills s WHERE s.agent_id=? AND s.bot_id=? AND s.version=(SELECT MAX(x.version) FROM agent_skills x WHERE x.agent_id=s.agent_id AND x.bot_id=s.bot_id AND x.name=s.name) ORDER BY s.name`
	if err := a.db.WithContext(ctx).Raw(q, agentID, botID).Scan(&rows).Error; err != nil {
		return nil, errors.New("listing skills failed")
	}
	return rows, nil
}

func (a *AgentStore) Skill(ctx context.Context, agentID string, botID int64, name string, activeOnly bool) (Skill, error) {
	var row Skill
	q := a.db.WithContext(ctx).Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=?", agentID, botID, name)
	if activeOnly {
		q = q.Where("status='active'")
	}
	if err := q.Order("version DESC").Take(&row).Error; err != nil {
		return row, errors.New("skill not found")
	}
	return row, nil
}

func (a *AgentStore) SkillVersions(ctx context.Context, agentID string, botID int64, name string) ([]Skill, error) {
	var rows []Skill
	if agentID == "" || botID <= 0 || !skillName.MatchString(name) {
		return rows, errors.New("invalid skill query")
	}
	if err := a.db.WithContext(ctx).Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=?", agentID, botID, name).Order("version DESC").Limit(50).Find(&rows).Error; err != nil {
		return nil, errors.New("listing skill versions failed")
	}
	return rows, nil
}

func (a *AgentStore) FinishSkillTest(ctx context.Context, skill Skill, input, output, failure string) error {
	status := "active"
	if failure != "" {
		status = "failed"
	}
	// pending skills wait for operator approval even when tests pass.
	if skill.Status == "pending" && status == "active" {
		status = "pending"
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if status == "active" {
			if err := tx.Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=? AND status='active'", skill.AgentID, skill.BotID, skill.Name).Update("status", "draft").Error; err != nil {
				return err
			}
		}
		result := tx.Table("agent_skills").Where("id=? AND status IN ('draft','pending')", skill.ID).Updates(map[string]any{"status": status, "test_input": input, "test_output": output, "error": failure, "updated_at": time.Now().UTC().UnixMilli()})
		if result.Error != nil || result.RowsAffected != 1 {
			return errors.New("skill version changed")
		}
		return nil
	})
}

// ApproveSkill moves a pending skill to draft so the bot can test and activate it.
func (a *AgentStore) ApproveSkill(ctx context.Context, agentID string, botID int64, name string, version int64) error {
	if agentID == "" || botID <= 0 || !skillName.MatchString(name) || version <= 0 {
		return errors.New("invalid skill approval")
	}
	result := a.db.WithContext(ctx).Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=? AND version=? AND status='pending'", agentID, botID, name, version).Update("status", "draft")
	if result.Error != nil {
		return errors.New("approval failed")
	}
	if result.RowsAffected != 1 {
		return errors.New("skill not found or not pending")
	}
	return nil
}

// RejectSkill removes a pending skill version.
func (a *AgentStore) RejectSkill(ctx context.Context, agentID string, botID int64, name string, version int64) error {
	if agentID == "" || botID <= 0 || !skillName.MatchString(name) || version <= 0 {
		return errors.New("invalid skill rejection")
	}
	result := a.db.WithContext(ctx).Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=? AND version=? AND status='pending'", agentID, botID, name, version).Delete(&Skill{})
	if result.Error != nil {
		return errors.New("rejection failed")
	}
	if result.RowsAffected != 1 {
		return errors.New("skill not found or not pending")
	}
	return nil
}

func (a *AgentStore) DeleteSkill(ctx context.Context, agentID string, botID int64, name string) error {
	if !skillName.MatchString(name) {
		return errors.New("invalid skill name")
	}
	return a.db.WithContext(ctx).Table("agent_skills").Where("agent_id=? AND bot_id=? AND name=?", agentID, botID, name).Delete(&Skill{}).Error
}

func cleanVirtualPath(raw string) (string, error) {
	if len(raw) > 512 || !strings.HasPrefix(raw, "/") || strings.Contains(raw, "\x00") {
		return "", errors.New("invalid virtual path")
	}
	for _, segment := range strings.Split(raw, "/") {
		if segment == ".." {
			return "", errors.New("invalid virtual path")
		}
	}
	return strings.TrimPrefix(path.Clean(raw), "/"), nil
}

func virtualScope(q *gorm.DB, scope chat.Scope) *gorm.DB {
	return q.Where("agent_id=? AND namespace='chat' AND bot_id=? AND chat_id=? AND user_id=0", scope.AgentID, scope.BotID, scope.ChatID)
}

func (a *AgentStore) ReadVirtualFile(ctx context.Context, scope chat.Scope, userID int64, rawPath string) (VirtualFile, error) {
	var row VirtualFile
	filePath, err := cleanVirtualPath(rawPath)
	if err != nil {
		return row, errors.New("invalid virtual file")
	}
	if err := virtualScope(a.db.WithContext(ctx).Table("virtual_files"), scope).Where("path=?", filePath).Take(&row).Error; err != nil {
		return row, errors.New("virtual file not found")
	}
	return row, nil
}

func (a *AgentStore) ListVirtualFiles(ctx context.Context, scope chat.Scope, prefix string) ([]VirtualFile, error) {
	var rows []VirtualFile
	if len(prefix) > 512 {
		return rows, errors.New("invalid virtual file query")
	}
	q := virtualScope(a.db.WithContext(ctx).Table("virtual_files"), scope)
	if prefix = strings.Trim(strings.TrimSpace(prefix), "/"); prefix != "" {
		q = q.Where("path LIKE ?", prefix+"%")
	}
	if err := q.Order("path").Limit(500).Find(&rows).Error; err != nil {
		return nil, errors.New("listing virtual files failed")
	}
	return rows, nil
}

// ErrVFSQuota is returned when a write would exceed the per-namespace byte limit.
var ErrVFSQuota = errors.New("virtual storage quota exceeded")

// ApplyVirtualChanges writes or deletes virtual files in one transaction.
// maxNamespaceBytes, when non-zero, enforces a total byte limit per namespace.
func (a *AgentStore) ApplyVirtualChanges(ctx context.Context, scope chat.Scope, userID int64, changes []VirtualFileChange, maxNamespaceBytes ...int) error {
	if len(changes) > 100 {
		return errors.New("too many virtual file changes")
	}
	limit := 0
	if len(maxNamespaceBytes) > 0 {
		limit = maxNamespaceBytes[0]
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		anyWrite := false
		for _, change := range changes {
			filePath, err := cleanVirtualPath(change.Path)
			if err != nil {
				return errors.New("invalid virtual file change: " + change.Path)
			}
			q := virtualScope(tx.Table("virtual_files"), scope).Where("path=?", filePath)
			if change.Content == nil {
				if err := q.Delete(&VirtualFile{}).Error; err != nil {
					return err
				}
				continue
			}
			if len(*change.Content) > 256000 {
				return errors.New("virtual file exceeds the 256 KB limit: " + change.Path)
			}
			now := time.Now().UTC().UnixMilli()
			row := map[string]any{"agent_id": scope.AgentID, "bot_id": scope.BotID, "chat_id": scope.ChatID, "user_id": 0, "namespace": "chat", "path": filePath, "content": *change.Content, "mime_type": "text/plain", "version": 1, "created_at": now, "updated_at": now}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "agent_id"}, {Name: "bot_id"}, {Name: "chat_id"}, {Name: "user_id"}, {Name: "namespace"}, {Name: "path"}}, DoUpdates: clause.Assignments(map[string]any{"content": *change.Content, "version": gorm.Expr("virtual_files.version+1"), "updated_at": now})}).Table("virtual_files").Create(row).Error; err != nil {
				return err
			}
			anyWrite = true
		}
		if limit > 0 && anyWrite {
			var row struct {
				Bytes int64
			}
			if err := virtualScope(tx.Table("virtual_files"), scope).
				Select("COALESCE(SUM(LENGTH(content)), 0) AS bytes").Scan(&row).Error; err != nil {
				return err
			}
			if row.Bytes > int64(limit) {
				return fmt.Errorf("%w: chat namespace uses %d bytes, limit is %d", ErrVFSQuota, row.Bytes, limit)
			}
		}
		return nil
	})
}

func VirtualPaths(rows []VirtualFile) []string {
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, "/"+row.Path)
	}
	sort.Strings(paths)
	return paths
}

// VirtualFileStat holds file metadata without content.
type VirtualFileStat struct {
	Path      string `json:"path"`
	Namespace string `json:"namespace"`
	Bytes     int64  `json:"bytes"`
	Version   int64  `json:"version"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func (a *AgentStore) StatVirtualFile(ctx context.Context, scope chat.Scope, userID int64, rawPath string) (VirtualFileStat, error) {
	var row VirtualFileStat
	filePath, err := cleanVirtualPath(rawPath)
	if err != nil {
		return row, errors.New("invalid virtual file")
	}
	if err := virtualScope(a.db.WithContext(ctx).Table("virtual_files"), scope).
		Select("path, namespace, LENGTH(content) AS bytes, version, created_at, updated_at").
		Where("path=?", filePath).Take(&row).Error; err != nil {
		return row, errors.New("virtual file not found")
	}
	return row, nil
}

// VirtualNamespaceUsage holds file count and total bytes for one namespace.
type VirtualNamespaceUsage struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

func (a *AgentStore) VirtualUsage(ctx context.Context, scope chat.Scope, userID int64) (VirtualNamespaceUsage, error) {
	var row struct {
		Files int64
		Bytes int64
	}
	if err := virtualScope(a.db.WithContext(ctx).Table("virtual_files"), scope).
		Select("COUNT(*) AS files, COALESCE(SUM(LENGTH(content)), 0) AS bytes").Scan(&row).Error; err != nil {
		return VirtualNamespaceUsage{}, errors.New("virtual usage query failed")
	}
	return VirtualNamespaceUsage{Files: row.Files, Bytes: row.Bytes}, nil
}
