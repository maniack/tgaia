package webui

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/maniack/tgaia/internal/telegram"
	"github.com/maniack/tgaia/internal/telemetry"
	"github.com/maniack/tgaia/internal/webauth"
	"log/slog"
)

//go:embed assets/*
var assets embed.FS

type Server struct {
	Monitor       *telemetry.Monitor
	RuntimeStatus func() any
	DB            *storage.Database
	Auth          *webauth.Auth
	Config        config.App
	Base          config.App
	Settings      *storage.Settings
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Referrer-Policy", "no-referrer")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
			next.ServeHTTP(w, r)
		})
	})
	if s.Config.Role == "worker" {
		return withRequestID(r)
	}
	r.Get("/login", s.Auth.Login)
	r.Get("/auth/callback", s.Auth.Callback)
	if s.Config.TelegramMode == "webhook" && s.Config.TelegramWebhookListen == "" {
		r.Post("/telegram/{bot}", s.webhook)
	}
	r.Group(func(r chi.Router) {
		r.Use(s.Auth.Protect)
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					group, _ := webauth.Session(r)
					slog.InfoContext(r.Context(), "admin mutation requested", "group", group, "operation", r.URL.Path)
				}
				next.ServeHTTP(w, r)
			})
		})
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			data, err := assets.ReadFile("assets/index.html")
			if err != nil {
				http.Error(w, "asset unavailable", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(data)
		})
		r.Get("/assets/{name}", func(w http.ResponseWriter, r *http.Request) {
			name := chi.URLParam(r, "name")
			if name != "app.js" && name != "style.css" {
				http.NotFound(w, r)
				return
			}
			data, err := assets.ReadFile("assets/" + name)
			if err != nil {
				http.Error(w, "asset unavailable", http.StatusInternalServerError)
				return
			}
			if strings.HasSuffix(name, ".js") {
				w.Header().Set("Content-Type", "text/javascript")
			} else {
				w.Header().Set("Content-Type", "text/css")
			}
			// #nosec G705 -- name is an explicit two-value allowlist and data comes
			// from the compile-time embedded filesystem, never from request input.
			_, _ = w.Write(data)
		})
		r.Get("/api/state", s.state)
		r.Get("/api/status", s.status)
		r.Get("/api/lists/{kind}", s.list)
		r.Post("/api/resolve-outcome", s.mutation((*Server).resolveOutcome))
		r.Post("/api/queue-replay", s.mutation((*Server).queueReplay))
		r.Get("/api/runtime", func(w http.ResponseWriter, r *http.Request) {
			if s.RuntimeStatus != nil {
				send(w, s.RuntimeStatus())
			} else {
				send(w, map[string]string{"state": "applied"})
			}
		})
		r.Get("/api/mcp", s.mcpServers)
		r.Post("/api/mcp", s.mutation((*Server).putMCP))
		r.Delete("/api/mcp", s.mutation((*Server).deleteMCP))
		r.Post("/api/mcp/probe", s.probeMCP)
		r.Post("/api/message-action", s.mutation((*Server).messageAction))
		r.Get("/api/context", s.context)
		r.Get("/api/user-profile", s.userProfile)
		r.Get("/api/skills", s.skills)
		r.Get("/api/vfs", s.virtualFiles)
		r.Delete("/api/memory", s.mutation((*Server).deleteMemory))
		r.Post("/api/chat-block", s.mutation((*Server).chatBlock))
		r.Post("/api/user-block", s.mutation((*Server).userBlock))
		r.Post("/api/user-role", s.mutation((*Server).userRole))
		r.Post("/api/tasks", s.mutation((*Server).task))
		r.Delete("/api/tasks", s.mutation((*Server).deleteTask))
		r.Get("/api/settings", s.settings)
		r.Post("/api/settings", s.mutation((*Server).putSetting))
		r.Delete("/api/settings", s.mutation((*Server).deleteSetting))
		r.Get("/api/prompt", s.prompt)
		r.Post("/api/prompt", s.mutation((*Server).putPrompt))
		r.Get("/api/chat-behavior", s.chatBehavior)
		r.Post("/api/chat-behavior", s.mutation((*Server).putChatBehavior))
		r.Get("/api/knowledge", s.knowledge)
		r.Post("/api/knowledge", s.mutation((*Server).putKnowledge))
		if s.Config.TelegramMode == "mock" {
			r.Post("/api/demo-message", s.mutation((*Server).demoMessage))
		}
	})
	return withRequestID(r)
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", uuid.NewString())
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ObservabilityHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	})
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.DB.Agent().Ready(r.Context()) != nil || (s.Config.Role != "api" && s.Monitor != nil && !s.Monitor.WorkerRunning.Load()) {
			http.Error(w, "schema unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready")
	})
	if s.Monitor != nil {
		r.Get("/metrics", func(w http.ResponseWriter, r *http.Request) {
			rows, err := s.DB.QueueMetrics(r.Context())
			if err != nil {
				http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
				return
			}
			s.Monitor.Metrics(w, r)
			fmt.Fprintln(w, "# TYPE tgaia_queue_jobs gauge")
			for _, row := range rows {
				fmt.Fprintf(w, "tgaia_queue_jobs{status=%q} %d\n", row.Status, row.Count)
			}
		})
	}
	return r
}

func (s *Server) WebhookHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	})
	if s.Config.TelegramMode == "webhook" {
		r.Post("/telegram/{bot}", s.webhook)
	}
	return r
}
func send(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func read(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, "Invalid JSON request", 400)
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		http.Error(w, "Trailing request data", 400)
		return false
	}
	return true
}
func result(w http.ResponseWriter, err error) {
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	send(w, map[string]bool{"ok": true})
}
func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	d, err := s.DB.Agent().Dashboard(r.Context())
	if err != nil {
		http.Error(w, "Cannot load dashboard", 503)
		return
	}
	group, csrf := webauth.Session(r)
	send(w, map[string]any{"data": d, "group": group, "csrf": csrf, "mock_telegram": s.Config.TelegramMode == "mock", "mock_llm": s.Config.MockLLM, "mock_oidc": s.Config.OIDCMode == "mock"})
}
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	data, err := s.DB.Agent().OperationalStatus(r.Context())
	if err != nil {
		http.Error(w, "Status unavailable", http.StatusServiceUnavailable)
		return
	}
	runtime := any(map[string]string{"state": "applied"})
	if s.RuntimeStatus != nil {
		runtime = s.RuntimeStatus()
	}
	enabledMCP := 0
	for _, server := range s.Config.MCPServers {
		if server.Enabled {
			enabledMCP++
		}
	}
	workerState, workerDetail := "ready", "Queue worker is running"
	if s.Config.Role == "api" {
		workerState, workerDetail = "conditional", "Handled by separate worker replicas"
	} else if s.Monitor != nil && !s.Monitor.WorkerRunning.Load() {
		workerState, workerDetail = "off", "Queue worker is not running"
	}
	capabilities := []map[string]any{
		{"name": "Language model", "state": "ready", "detail": map[bool]string{true: "Deterministic local mock", false: "OpenAI-compatible model " + s.Config.LLMModel}[s.Config.MockLLM]},
		{"name": "Telegram transport", "state": "ready", "detail": map[string]string{"mock": "Local Telegram simulator", "polling": "Polling", "webhook": "Webhook"}[s.Config.TelegramMode]},
		{"name": "Queue worker", "state": workerState, "detail": workerDetail},
		{"name": "Agent tools", "state": "ready", "detail": "Tasks, memory and Telegram message CRUD"},
		{"name": "Scheduler", "state": "ready", "detail": "Durable one-time and recurring tasks"},
		{"name": "Automatic profiles", "state": "ready", "detail": "Conservative fact extraction with deduplication"},
		{"name": "Memory compaction", "state": "ready", "detail": fmt.Sprintf("Window %s; scan interval %s", s.Config.MemoryWindow, s.Config.MemoryInterval)},
		{"name": "Image understanding", "state": "ready", "detail": "Descriptions feed history and compaction"},
		{"name": "Agent-written skills", "state": "ready", "detail": "Versioned Starlark sandbox with automatic test activation"},
		{"name": "Virtual filesystem", "state": "ready", "detail": "Database-backed /user, /chat and /bot namespaces"},
		{"name": "Authentication", "state": "ready", "detail": map[bool]string{true: "Local mock OIDC", false: "OpenID Connect"}[s.Config.OIDCMode == "mock"]},
		{"name": "Group moderation", "state": map[bool]string{true: "conditional", false: "off"}[s.Config.TelegramAdminTools], "detail": map[bool]string{true: "Each action also requires live Telegram administrator rights", false: "Enable telegram.admin_tools to allow permission checks"}[s.Config.TelegramAdminTools]},
		{"name": "External integrations", "state": map[bool]string{true: "ready", false: "off"}[enabledMCP > 0], "detail": fmt.Sprintf("%d enabled MCP servers", enabledMCP)},
	}
	send(w, map[string]any{"generated_at": time.Now().UTC().Format(time.RFC3339), "database": "ready", "runtime": runtime, "activity": data, "capabilities": capabilities})
}
func scope(r *http.Request) chat.Scope {
	bot, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	threadID, _ := strconv.ParseInt(r.URL.Query().Get("thread_id"), 10, 64)
	return chat.Scope{AgentID: r.URL.Query().Get("agent_id"), BotID: bot, ChatID: chatID, ThreadID: threadID}
}
func (s *Server) context(w http.ResponseWriter, r *http.Request) {
	value, err := s.DB.Agent().Context(r.Context(), scope(r), 0, 50)
	if err != nil {
		http.Error(w, "Context unavailable", 400)
		return
	}
	send(w, value)
}
func (s *Server) skills(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	agentID, name := r.URL.Query().Get("agent_id"), r.URL.Query().Get("name")
	if name != "" {
		rows, err := s.DB.Agent().SkillVersions(r.Context(), agentID, botID, name)
		if err != nil {
			http.Error(w, "Skills unavailable", 400)
			return
		}
		send(w, map[string]any{"versions": rows})
		return
	}
	rows, err := s.DB.Agent().Skills(r.Context(), agentID, botID)
	if err != nil {
		http.Error(w, "Skills unavailable", 400)
		return
	}
	send(w, map[string]any{"skills": rows})
}
func (s *Server) virtualFiles(w http.ResponseWriter, r *http.Request) {
	sc := scope(r)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if path := r.URL.Query().Get("path"); path != "" {
		file, err := s.DB.Agent().ReadVirtualFile(r.Context(), sc, userID, path)
		if err != nil {
			http.Error(w, "Virtual file unavailable", 404)
			return
		}
		send(w, map[string]any{"file": file})
		return
	}
	rows, err := s.DB.Agent().ListVirtualFiles(r.Context(), sc, userID, r.URL.Query().Get("namespace"), r.URL.Query().Get("prefix"))
	if err != nil {
		http.Error(w, "Virtual files unavailable", 400)
		return
	}
	send(w, map[string]any{"files": rows})
}
func (s *Server) userProfile(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	value, err := s.DB.Agent().UserProfileMemories(r.Context(), r.URL.Query().Get("agent_id"), userID)
	if err != nil {
		http.Error(w, "Profile unavailable", http.StatusBadRequest)
		return
	}
	send(w, value)
}
func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID         string `json:"agent_id"`
		BotID           int64  `json:"bot_id"`
		ChatID          int64  `json:"chat_id"`
		ThreadID        int64  `json:"thread_id"`
		SourceMessageID int64  `json:"source_message_id"`
	}
	if !read(w, r, &in) {
		return
	}
	result(w, s.DB.Agent().AdminDeleteExplicitMemory(r.Context(), chat.Scope{AgentID: in.AgentID, BotID: in.BotID, ChatID: in.ChatID, ThreadID: in.ThreadID}, in.SourceMessageID))
}
func (s *Server) chatBlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		BotID   int64  `json:"bot_id"`
		ChatID  int64  `json:"chat_id"`
		Blocked bool   `json:"blocked"`
	}
	if !read(w, r, &in) {
		return
	}
	result(w, s.DB.Agent().BlockChat(r.Context(), chat.Scope{AgentID: in.AgentID, BotID: in.BotID, ChatID: in.ChatID}, in.Blocked))
}
func (s *Server) userBlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		UserID  int64  `json:"user_id"`
		Blocked bool   `json:"blocked"`
	}
	if !read(w, r, &in) {
		return
	}
	_, err := s.DB.Access().SetUserBlocked(r.Context(), in.AgentID, in.UserID, in.Blocked)
	result(w, err)
}
func (s *Server) task(w http.ResponseWriter, r *http.Request) {
	var in agent.Task
	if !read(w, r, &in) {
		return
	}
	if in.ID == "" {
		in.ID = uuid.NewString()
	}
	result(w, s.DB.Agent().PutTask(r.Context(), in, in.Version, s.Config.Limit("active_tasks_per_user"), s.Config.Limit("system_prompt_bytes")))
}
func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if !read(w, r, &in) {
		return
	}
	result(w, s.DB.Agent().DeleteTask(r.Context(), in.ID, in.Version))
}

// UI connection settings are managed out of band to prevent locking administrators out.
func protectedUISetting(name string) bool {
	return name == "listen" || name == "public_url" || name == "role"
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	scoped, err := s.Settings.InScope(r.URL.Query().Get("scope"))
	if err != nil {
		result(w, err)
		return
	}
	_, overrides, err := scoped.Load(r.Context())
	if err != nil {
		http.Error(w, "Settings unavailable", 503)
		return
	}
	rows := []map[string]any{}
	versions := map[string]storage.Setting{}
	for _, row := range overrides {
		versions[row.Name] = row
	}
	base := s.Base.Settings()
	effective := s.Base.Settings()
	resolved, _, err := s.Settings.LoadResolved(r.Context(), scoped.Scope())
	if err != nil {
		result(w, err)
		return
	}
	for k, v := range resolved {
		effective[k] = v
	}
	if cfg, err := s.Base.WithSettings(resolved); err == nil {
		effective = cfg.Settings()
	}
	for key, value := range base {
		if protectedUISetting(key) || !config.AllowedSettingScope(scoped.Scope(), key) {
			continue
		}
		override, ok := versions[key]
		current := effective[key]
		if config.SecretSetting(key) {
			value = ""
			current = ""
		}
		rows = append(rows, map[string]any{"scope": scoped.Scope(), "name": key, "file_value": value, "effective_value": current, "override_value": override.Value, "secret": config.SecretSetting(key), "overridden": ok, "version": override.Version, "restart_required": false})
	}
	send(w, rows)
}
func (s *Server) putSetting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope   string `json:"scope"`
		Name    string `json:"name"`
		Value   string `json:"value"`
		Version int64  `json:"version"`
	}
	if !read(w, r, &in) {
		return
	}
	if protectedUISetting(in.Name) {
		http.Error(w, "Setting is managed outside the Web UI", http.StatusForbidden)
		return
	}
	scoped, err := s.Settings.InScope(in.Scope)
	if err != nil {
		result(w, err)
		return
	}
	if !config.AllowedSettingScope(scoped.Scope(), in.Name) {
		http.Error(w, "setting not allowed in this scope", 400)
		return
	}
	if err := s.validateSettingChange(r.Context(), scoped.Scope(), in.Name, &in.Value); err != nil {
		result(w, err)
		return
	}
	result(w, scoped.Put(r.Context(), in.Name, in.Value, in.Version))
}
func (s *Server) deleteSetting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope   string `json:"scope"`
		Name    string `json:"name"`
		Version int64  `json:"version"`
	}
	if !read(w, r, &in) {
		return
	}
	if protectedUISetting(in.Name) {
		http.Error(w, "Setting is managed outside the Web UI", http.StatusForbidden)
		return
	}
	scoped, err := s.Settings.InScope(in.Scope)
	if err != nil {
		result(w, err)
		return
	}
	if err := s.validateSettingChange(r.Context(), scoped.Scope(), in.Name, nil); err != nil {
		result(w, err)
		return
	}
	if !config.AllowedSettingScope(scoped.Scope(), in.Name) {
		http.Error(w, "setting not allowed in this scope", 400)
		return
	}
	result(w, scoped.Delete(r.Context(), in.Name, in.Version))
}
func (s *Server) prompt(w http.ResponseWriter, r *http.Request) {
	sc := scope(r)
	value, err := s.DB.Bots().Prompt(r.Context(), sc.AgentID, sc.BotID)
	if err != nil {
		result(w, err)
		return
	}
	send(w, map[string]string{"prompt": value})
}
func (s *Server) putPrompt(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		BotID   int64  `json:"bot_id"`
		Prompt  string `json:"prompt"`
	}
	if !read(w, r, &in) {
		return
	}
	result(w, s.DB.Bots().SetPrompt(r.Context(), in.AgentID, in.BotID, in.Prompt, s.Config.Limit("system_prompt_bytes")))
}
func (s *Server) chatBehavior(w http.ResponseWriter, r *http.Request) {
	behavior, err := s.DB.Agent().ChatBehavior(r.Context(), scope(r))
	if err != nil {
		result(w, err)
		return
	}
	send(w, behavior)
}
func (s *Server) putChatBehavior(w http.ResponseWriter, r *http.Request) {
	var behavior agent.ChatBehavior
	if !read(w, r, &behavior) {
		return
	}
	result(w, s.DB.Agent().SetChatBehavior(r.Context(), behavior))
}
func (s *Server) knowledge(w http.ResponseWriter, r *http.Request) {
	value, version, err := s.DB.Agent().Knowledge(r.Context(), r.URL.Query().Get("agent_id"))
	if err != nil {
		result(w, err)
		return
	}
	send(w, map[string]any{"content": value, "version": version})
}
func (s *Server) putKnowledge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		Content string `json:"content"`
		Version int64  `json:"version"`
	}
	if !read(w, r, &in) {
		return
	}
	result(w, s.DB.Agent().PutKnowledge(r.Context(), in.AgentID, in.Content, in.Version, s.Config.Limit("system_prompt_bytes")))
}
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "bot"), 10, 64)
	var bot *config.Bot
	for i := range s.Config.Bots {
		if s.Config.Bots[i].ID == id {
			bot = &s.Config.Bots[i]
		}
	}
	if bot == nil || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Telegram-Bot-Api-Secret-Token")), []byte(bot.WebhookSecret.Reveal())) != 1 {
		http.Error(w, "Unauthorized webhook", 401)
		return
	}
	// Telegram may add fields. The transport decoder intentionally accepts them.
	var update telegram.Update
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if json.NewDecoder(r.Body).Decode(&update) != nil {
		http.Error(w, "Invalid update", 400)
		return
	}
	if reaction, ok := telegram.ReactionUpdate(update, s.Config.AgentID, id); ok {
		if err := s.DB.Agent().ApplyReaction(r.Context(), reaction); err != nil {
			http.Error(w, "Persistence unavailable", 503)
			return
		}
	} else if incoming, ok := telegram.Incoming(update, s.Config.AgentID, id, bot.Username); ok {
		if err := s.DB.Agent().Accept(r.Context(), incoming); err != nil {
			http.Error(w, "Persistence unavailable", 503)
			return
		}
	}
	w.WriteHeader(200)
}
func (s *Server) demoMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		BotID        int64  `json:"bot_id"`
		ChatID       int64  `json:"chat_id"`
		UserID       int64  `json:"user_id"`
		ReplyMessage int64  `json:"reply_to_message_id"`
		Name         string `json:"name"`
		Username     string `json:"username"`
		LanguageCode string `json:"language_code"`
		IsPremium    bool   `json:"is_premium"`
		IsBot        bool   `json:"is_bot"`
		ChatType     string `json:"chat_type"`
		Text         string `json:"text"`
	}
	if !read(w, r, &in) {
		return
	}
	found := false
	for _, bot := range s.Config.Bots {
		if bot.ID == in.BotID {
			found = true
		}
	}
	if in.ChatType == "" {
		in.ChatType = "private"
	}
	if !found || in.UserID <= 0 || in.ReplyMessage < 0 || len(in.Name) > 128 || len(in.Username) > 64 || len(in.LanguageCode) > 16 || (in.ChatType != "private" && in.ChatType != "group" && in.ChatType != "supergroup") {
		http.Error(w, "Invalid demo identity", 400)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = fmt.Sprintf("Local user %d", in.UserID)
	}
	id := time.Now().UnixMicro()
	result(w, s.DB.Agent().Accept(r.Context(), agent.Incoming{Scope: chat.Scope{AgentID: s.Config.AgentID, BotID: in.BotID, ChatID: in.ChatID}, UpdateID: id, MessageID: id, UserID: in.UserID, ReplyMessageID: in.ReplyMessage, Name: name, Username: strings.TrimPrefix(strings.TrimSpace(in.Username), "@"), LanguageCode: strings.TrimSpace(in.LanguageCode), IsPremium: in.IsPremium, IsBot: in.IsBot, ChatType: in.ChatType, Title: "Local test chat", Text: in.Text, SentAt: time.Now().UTC(), Respond: in.ChatType == "private"}))
}

func (s *Server) messageAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID   string `json:"agent_id"`
		BotID     int64  `json:"bot_id"`
		ThreadID  int64  `json:"thread_id"`
		ChatID    int64  `json:"chat_id"`
		MessageID int64  `json:"message_id"`
		Action    string `json:"action"`
		Text      string `json:"text"`
	}
	if !read(w, r, &in) {
		return
	}
	sc := chat.Scope{AgentID: in.AgentID, BotID: in.BotID, ChatID: in.ChatID, ThreadID: in.ThreadID}
	if in.Action != "edit" && in.Action != "delete" {
		http.Error(w, "Invalid action", 400)
		return
	}
	if _, err := s.DB.BotMessages().Get(r.Context(), messages.Reference{Scope: sc, MessageID: in.MessageID}); err != nil {
		result(w, err)
		return
	}
	payload, _ := json.Marshal(agent.JobPayload{Action: in.Action, Text: in.Text, MessageID: in.MessageID})
	_, err := s.DB.Queue().Enqueue(r.Context(), queue.Input{Scope: sc, Key: "admin-message:" + uuid.NewString(), Kind: "message_action", Payload: payload, MaxAttempts: 3})
	result(w, err)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil {
			http.Error(w, "Invalid page size", 400)
			return
		}
		limit = n
	}
	page, err := s.DB.Agent().AdminPage(r.Context(), chi.URLParam(r, "kind"), r.URL.Query().Get("cursor"), limit, scope(r))
	if err != nil {
		result(w, err)
		return
	}
	send(w, page)
}
func (s *Server) resolveOutcome(w http.ResponseWriter, r *http.Request) {
	var in storage.Resolution
	if !read(w, r, &in) {
		return
	}
	group, _ := webauth.Session(r)
	err := s.DB.Agent().ResolveOutcome(r.Context(), in, group)
	if errors.Is(err, storage.ErrResolutionConflict) {
		http.Error(w, err.Error(), 409)
		return
	}
	result(w, err)
}

func (s *Server) queueReplay(w http.ResponseWriter, r *http.Request) {
	var in struct {
		JobID int64 `json:"job_id"`
	}
	if !read(w, r, &in) {
		return
	}
	err := s.DB.Queue().Replay(r.Context(), in.JobID)
	if errors.Is(err, storage.ErrNotDead) {
		http.Error(w, err.Error(), 409)
		return
	}
	result(w, err)
}

func (s *Server) userRole(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		UserID  int64  `json:"user_id"`
		Role    string `json:"role"`
		Version int64  `json:"version"`
	}
	if !read(w, r, &in) {
		return
	}
	result(w, s.DB.Agent().SetRole(r.Context(), in.AgentID, in.UserID, in.Role, in.Version))
}
