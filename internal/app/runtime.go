package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/devmode"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/service"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/maniack/tgaia/internal/telegram"
	"github.com/maniack/tgaia/internal/telemetry"
	"github.com/maniack/tgaia/internal/webauth"
	"github.com/maniack/tgaia/internal/webui"
)

const settingsPoll = 250 * time.Millisecond

type snapshot struct {
	apiConfig config.App
	config    config.App
	engine    *service.Engine
	pollers   map[int64]*telegram.Client
	auth      *webauth.Auth
}
type applyStatus struct {
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	AppliedAt time.Time `json:"applied_at,omitempty"`
}
type runtime struct {
	monitor  *telemetry.Monitor
	base     config.App
	db       *storage.Database
	settings *storage.Settings
	issuer   string
	current  atomic.Pointer[snapshot]
	statusMu sync.RWMutex
	status   applyStatus
	level    slog.LevelVar
	log      *slog.Logger
}

func (r *runtime) Status() any { r.statusMu.RLock(); defer r.statusMu.RUnlock(); return r.status }
func (r *runtime) setStatus(state string, err error) {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	r.status.State = state
	r.status.Error = ""
	if err != nil {
		r.status.Error = err.Error()
	}
	if state == "applied" {
		r.status.AppliedAt = time.Now().UTC()
	}
}
func (r *runtime) prepare(ctx context.Context, values map[string]string) (*snapshot, error) {
	cfg, err := r.base.WithSettings(values)
	if err != nil {
		return nil, err
	}
	if err = r.db.Agent().BindEnvironment(ctx, cfg.TelegramMode == "mock"); err != nil {
		return nil, err
	}
	apiCfg := cfg
	if r.base.Role == "all" && r.settings != nil {
		apiValues, _, err := r.settings.LoadResolved(ctx, "api")
		if err != nil {
			return nil, err
		}
		apiCfg, err = r.base.WithSettings(apiValues)
		if err != nil {
			return nil, err
		}
	}
	next := &snapshot{apiConfig: apiCfg, config: cfg, engine: &service.Engine{Config: cfg, Identities: map[int64]service.BotIdentity{}, DefaultPromptFile: cfg.DefaultPromptFile, DB: r.db, Bots: map[int64]service.Sender{}}, pollers: map[int64]*telegram.Client{}}
	old := r.current.Load()
	if old != nil && old.config.MockLLM == cfg.MockLLM && old.config.LLMEndpoint == cfg.LLMEndpoint && old.config.LLMModel == cfg.LLMModel && old.config.LLMKey.Reveal() == cfg.LLMKey.Reveal() && maps.Equal(old.config.Limits, cfg.Limits) {
		next.engine.Model = old.engine.Model
	} else if cfg.MockLLM {
		next.engine.Model = devmode.Model{}
	} else {
		next.engine.Model, err = llm.NewWithLimits(cfg.LLMEndpoint, cfg.LLMModel, cfg.LLMKey, cfg)
		if err != nil {
			return nil, err
		}
	}
	if cfg.Role != "api" && cfg.EmbeddingModel != "" {
		if old != nil && old.config.EmbeddingModel == cfg.EmbeddingModel && old.config.MockLLM == cfg.MockLLM && old.config.LLMEndpoint == cfg.LLMEndpoint && old.config.LLMKey.Reveal() == cfg.LLMKey.Reveal() && maps.Equal(old.config.Limits, cfg.Limits) {
			next.engine.Embedder = old.engine.Embedder
		} else if cfg.MockLLM {
			next.engine.Embedder = devmode.Embedder{Name: cfg.EmbeddingModel}
		} else {
			next.engine.Embedder, err = llm.NewEmbedder(cfg.LLMEndpoint, cfg.EmbeddingModel, cfg.LLMKey, cfg)
			if err != nil {
				return nil, err
			}
		}
	}
	for i := range next.config.Bots {
		bot := &next.config.Bots[i]
		if cfg.TelegramMode == "mock" {
			if old != nil && old.config.TelegramMode == "mock" && old.engine.Bots[bot.ID] != nil {
				next.engine.Bots[bot.ID] = old.engine.Bots[bot.ID]
			} else {
				next.engine.Bots[bot.ID] = &devmode.Sender{}
			}
		} else {
			var previous *config.Bot
			if old != nil && old.config.TelegramMode != "mock" {
				for _, b := range old.config.Bots {
					if b.Name == bot.Name && b.Token.Reveal() == bot.Token.Reveal() && old.config.Limit("image_bytes") == cfg.Limit("image_bytes") && old.config.Limit("document_bytes") == cfg.Limit("document_bytes") && old.config.Duration("telegram_timeout") == cfg.Duration("telegram_timeout") {
						copy := b
						previous = &copy
						break
					}
				}
			}
			if previous != nil {
				bot.ID = previous.ID
				bot.Username = previous.Username
				bot.DisplayName = previous.DisplayName
				next.pollers[bot.ID] = old.pollers[bot.ID]
			} else {
				client := telegram.NewWithLimits(bot.Token, cfg)
				identity, err := client.Identity(ctx)
				if err != nil {
					return nil, err
				}
				bot.ID = identity.ID
				bot.Username = identity.Username
				bot.DisplayName = identity.FirstName
				next.pollers[bot.ID] = client
			}
			next.engine.Bots[bot.ID] = next.pollers[bot.ID]
			if apiCfg.Duration("telegram_timeout") != cfg.Duration("telegram_timeout") {
				next.pollers[bot.ID] = telegram.NewWithLimits(bot.Token, apiCfg)
			}
		}
		if cfg.TelegramMode == "mock" {
			bot.DisplayName = bot.Name
		}
		next.engine.Identities[bot.ID] = service.BotIdentity{Name: bot.DisplayName, Username: bot.Username}
		if _, err = r.db.Bots().Register(ctx, cfg.AgentID, bot.ID, "CONFIG_BOT_"+strings.ToUpper(bot.Name)); err != nil {
			return nil, err
		}
	}
	aliases := map[int64]string{}
	for _, b := range next.config.Bots {
		aliases[b.ID] = b.Name
	}
	next.engine.Tools = &mcpclient.Provider{Config: cfg, AgentID: cfg.AgentID, Bots: aliases, Servers: cfg.MCPServers, Permit: func(scope chat.Scope, name, tool string) bool {
		active := r.current.Load()
		if active == nil || active.config.AgentID != cfg.AgentID {
			return false
		}
		botPresent := slices.ContainsFunc(active.config.Bots, func(b config.Bot) bool { return b.ID == scope.BotID && b.Name == aliases[scope.BotID] })
		if !botPresent {
			return false
		}
		for _, server := range cfg.MCPServers {
			if server.Name == name {
				for _, updated := range active.config.MCPServers {
					if updated.Name == name {
						// Any server replacement/revocation invalidates an old session before
						// another call. No old credentials or old grants are used after publication.
						return updated.Enabled && config.EncodeMCP([]config.MCPServer{server}) == config.EncodeMCP([]config.MCPServer{updated}) && (slices.Contains(updated.Tools, "*") || slices.Contains(updated.Tools, tool))
					}
				}
			}
		}
		return false
	}}
	if cfg.Role != "worker" {
		if old != nil && old.auth != nil && old.apiConfig.PublicURL == apiCfg.PublicURL {
			next.auth = old.auth
		} else {
			authCfg := webauth.Config{Issuer: r.issuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret.Reveal(), Callback: strings.TrimRight(apiCfg.PublicURL, "/") + "/auth/callback", SessionKey: cfg.SessionKey.Reveal(), AdminGroups: cfg.AdminGroups}
			if cfg.OIDCMode == "mock" {
				authCfg.AdminGroups = []string{"local-admins"}
			}
			next.auth, err = webauth.New(ctx, authCfg)
			if err != nil {
				return nil, err
			}
		}
	}
	return next, nil
}
func (r *runtime) handler(next *snapshot) http.Handler {
	return (&webui.Server{DB: r.db, Auth: next.auth, Config: next.apiConfig, Base: r.base, Settings: r.settings, RuntimeStatus: r.Status, Monitor: r.monitor}).Handler()
}
func (r *runtime) observabilityHandler(next *snapshot) http.Handler {
	return (&webui.Server{DB: r.db, Config: next.config, Monitor: r.monitor}).ObservabilityHandler()
}
func (r *runtime) webhookHandler(next *snapshot) http.Handler {
	return (&webui.Server{DB: r.db, Config: next.config}).WebhookHandler()
}
func (r *runtime) handlers() map[string]queue.Handler {
	handlers := map[string]queue.Handler{}
	for _, kind := range []string{"respond", "compact", "message_action"} {
		handlers[kind] = func(ctx context.Context, job queue.Job) error {
			current := r.current.Load()
			if current == nil || job.Scope.AgentID != current.config.AgentID {
				return errors.New("runtime is not available for this agent")
			}
			return r.monitor.Handle(ctx, job, current.engine.Handlers()[kind])
		}
	}
	return handlers
}

type handlerSlot struct{ value atomic.Pointer[http.Handler] }

func (s *handlerSlot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.value.Load()).ServeHTTP(w, r)
}
func (s *handlerSlot) Store(handler http.Handler) { s.value.Store(&handler) }

type httpInstance struct {
	server  *http.Server
	slot    *handlerSlot
	address string
}

func stageHTTP(address string, handler http.Handler) (*httpInstance, net.Listener, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, errors.New("cannot bind new listen address; previous runtime retained")
	}
	slot := &handlerSlot{}
	slot.Store(handler)
	return &httpInstance{address: address, slot: slot, server: &http.Server{Handler: slot, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 16}}, listener, nil
}

type runningGroup struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (g *runningGroup) stop() {
	if g == nil {
		return
	}
	g.cancel()
	<-g.done
}
func startGroup(ctx context.Context, fn func(context.Context)) *runningGroup {
	ctx, cancel := context.WithCancel(ctx)
	g := &runningGroup{cancel: cancel, done: make(chan struct{})}
	go func() { defer close(g.done); fn(ctx) }()
	return g
}
func pollSignature(s *snapshot) string {
	if s == nil || s.config.Role == "worker" || s.config.TelegramMode != "polling" {
		return ""
	}
	value := s.config.AgentID + fmt.Sprint(s.apiConfig.Duration("telegram_timeout"), s.config.Limit("image_bytes"), s.config.Limit("document_bytes"))
	for _, b := range s.config.Bots {
		value += fmt.Sprintf("/%s/%d/%s/%s", b.Name, b.ID, b.Username, b.Token.Reveal())
	}
	return value
}
func settingsDigest(values map[string]string) [32]byte {
	encoded, _ := json.Marshal(values)
	return sha256.Sum256(encoded)
}

func Run(parent context.Context, dbCfg config.Config, base config.App, out io.Writer) error {
	if err := base.Validate(); err != nil {
		return err
	}
	db, err := storage.Open(parent, dbCfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = db.Agent().Ready(parent); err != nil {
		return err
	}
	settings, err := db.Settings(base.SessionKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	r := &runtime{monitor: telemetry.New(), base: base, db: db, settings: settings, issuer: base.OIDCIssuer}
	defer r.monitor.Close()
	if base.OIDCMode == "mock" {
		issuer, close, err := devmode.OIDC(base.OIDCClientID)
		if err != nil {
			return errors.New("mock OIDC initialization failed")
		}
		defer close()
		r.issuer = issuer
	}
	r.log = slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: &r.level}))
	var appLive, observabilityLive, webhookLive *httpInstance
	var workers, polling *runningGroup
	var httpWG sync.WaitGroup
	faults := make(chan error, 4)
	stopHTTP := func(h *httpInstance) {
		if h == nil {
			return
		}
		httpWG.Add(1)
		go func() {
			defer httpWG.Done()
			shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if h.server.Shutdown(shutdown) != nil {
				_ = h.server.Close()
			}
		}()
	}
	defer func() {
		workers.stop()
		polling.stop()
		stopHTTP(appLive)
		stopHTTP(observabilityLive)
		stopHTTP(webhookLive)
		httpWG.Wait()
	}()
	publish := func(next *snapshot) error {
		previous := r.current.Load()
		if err := r.monitor.Configure(ctx, next.config.OTLPEndpoint); err != nil {
			return err
		}
		var stagedApp, stagedObservability, stagedWebhook *httpInstance
		var appListener, observabilityListener, webhookListener net.Listener
		if next.config.Role != "worker" && (appLive == nil || appLive.address != next.apiConfig.Listen) {
			var err error
			stagedApp, appListener, err = stageHTTP(next.apiConfig.Listen, r.handler(next))
			if err != nil {
				return err
			}
		}
		observabilityAddress := next.config.ObservabilityAddress()
		if observabilityLive == nil || observabilityLive.address != observabilityAddress {
			var err error
			stagedObservability, observabilityListener, err = stageHTTP(observabilityAddress, r.observabilityHandler(next))
			if err != nil {
				if appListener != nil {
					_ = appListener.Close()
				}
				return err
			}
		}
		webhookWanted := next.config.Role != "worker" && next.config.TelegramMode == "webhook" && next.config.TelegramWebhookListen != ""
		if webhookWanted && (webhookLive == nil || webhookLive.address != next.config.TelegramWebhookListen) {
			var err error
			stagedWebhook, webhookListener, err = stageHTTP(next.config.TelegramWebhookListen, r.webhookHandler(next))
			if err != nil {
				if appListener != nil {
					_ = appListener.Close()
				}
				if observabilityListener != nil {
					_ = observabilityListener.Close()
				}
				return err
			}
		}
		// Stop only consumers whose transport/role changed; other in-flight jobs
		// retain their immutable snapshot and new claims use the new one.
		if pollSignature(previous) != pollSignature(next) {
			polling.stop()
			polling = nil
		}
		r.current.Store(next)
		r.level.Set(next.config.LogLevel)
		startHTTP := func(instance *httpInstance, listener net.Listener, label string) {
			httpWG.Add(1)
			go func() {
				defer httpWG.Done()
				if err := instance.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					select {
					case faults <- errors.New(label + " HTTP server failed"):
					default:
					}
					cancel()
				}
			}()
		}
		if stagedApp != nil {
			old := appLive
			appLive = stagedApp
			startHTTP(stagedApp, appListener, "application")
			stopHTTP(old)
		} else if appLive != nil {
			appLive.slot.Store(r.handler(next))
		}
		if stagedObservability != nil {
			old := observabilityLive
			observabilityLive = stagedObservability
			startHTTP(stagedObservability, observabilityListener, "observability")
			stopHTTP(old)
		} else {
			observabilityLive.slot.Store(r.observabilityHandler(next))
		}
		if stagedWebhook != nil {
			old := webhookLive
			webhookLive = stagedWebhook
			startHTTP(stagedWebhook, webhookListener, "Telegram webhook")
			stopHTTP(old)
		} else if webhookWanted {
			webhookLive.slot.Store(r.webhookHandler(next))
		} else if webhookLive != nil {
			stopHTTP(webhookLive)
			webhookLive = nil
		}
		if previous != nil && previous.config.WorkerOptions() != next.config.WorkerOptions() {
			workers.stop()
			workers = nil
		}
		if next.config.Role == "api" {
			workers.stop()
			workers = nil
		} else if workers == nil {
			worker, err := queue.NewWorker(db.Queue(), r.handlers(), next.config.WorkerOptions(), r.log)
			if err != nil {
				return err
			}
			worker.OnDead(func(ctx context.Context, job queue.Job, class string) {
				if current := r.current.Load(); current != nil && job.Scope.AgentID == current.config.AgentID {
					current.engine.Failed(ctx, job, class)
				}
			})
			workers = startGroup(ctx, func(ctx context.Context) {
				var wg sync.WaitGroup
				wg.Add(1)
				go func() { defer wg.Done(); schedule(ctx, r) }()
				r.monitor.WorkerRunning.Store(true)
				if err := worker.Run(ctx); err != nil {
					select {
					case faults <- err:
					default:
					}
					cancel()
				}
				r.monitor.WorkerRunning.Store(false)
				wg.Wait()
			})
		}
		if polling == nil && pollSignature(next) != "" {
			polling = startGroup(ctx, func(ctx context.Context) {
				var wg sync.WaitGroup
				for _, bot := range next.config.Bots {
					wg.Add(1)
					go func() { defer wg.Done(); poll(ctx, db, next.config.AgentID, bot, next.pollers[bot.ID], r.log) }()
				}
				wg.Wait()
			})
		}
		r.setStatus("applied", nil)
		r.log.Info("runtime settings applied")
		return nil
	}
	values, _, err := settings.LoadResolved(ctx, base.Role)
	if err != nil {
		return err
	}
	prepareCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	first, err := r.prepare(prepareCtx, values)
	stop()
	if err != nil {
		return err
	}
	if err = publish(first); err != nil {
		return err
	}
	if first.config.Role != "worker" {
		fmt.Fprintf(out, "Web UI: %s (OIDC: %s; Telegram: %s; mock LLM: %t)\n", first.apiConfig.PublicURL, first.config.OIDCMode, first.config.TelegramMode, first.config.MockLLM)
	}
	applied := runtimeDigest(ctx, settings, base.Role, values)
	retryAt := time.Time{}
	ticker := time.NewTicker(settingsPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			select {
			case err := <-faults:
				return err
			default:
				return nil
			}
		case <-ticker.C:
			values, _, err := settings.LoadResolved(ctx, base.Role)
			if err != nil {
				r.setStatus("error", err)
				continue
			}
			desired := runtimeDigest(ctx, settings, base.Role, values)
			if desired == applied {
				if status := r.Status().(applyStatus); status.State != "applied" {
					r.setStatus("applied", nil)
				}
				continue
			}
			if time.Now().Before(retryAt) {
				continue
			}
			r.setStatus("applying", nil)
			prepareCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			next, err := r.prepare(prepareCtx, values)
			stop()
			if err == nil {
				err = publish(next)
			}
			if err != nil {
				r.setStatus("error", err)
				retryAt = time.Now().Add(time.Second)
				continue
			}
			applied = desired
			retryAt = time.Time{}
		}
	}
}
func schedule(ctx context.Context, r *runtime) {
	ticker := time.NewTicker(settingsPoll)
	defer ticker.Stop()
	next := time.Time{}
	var interval time.Duration
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := r.current.Load().config
			if cfg.MemoryInterval != interval {
				interval = cfg.MemoryInterval
				next = time.Time{}
			}
			if time.Now().Before(next) {
				continue
			}
			policy := storage.CompactionPolicy{MinMessages: cfg.Limit("compaction_min_messages"), MaxUncompact: cfg.Limit("reply_messages")}
			if err := r.db.Agent().Tick(ctx, cfg.MemoryWindow, policy); err != nil && ctx.Err() == nil {
				r.log.Error("scheduler tick failed")
			}
			next = time.Now().Add(interval)
		}
	}
}

func runtimeDigest(ctx context.Context, s *storage.Settings, role string, values map[string]string) [32]byte {
	merged := maps.Clone(values)
	if role == "all" {
		api, _, err := s.LoadResolved(ctx, "api")
		if err != nil {
			merged["api:error"] = err.Error()
		} else {
			for k, v := range api {
				merged["api:"+k] = v
			}
		}
	}
	return settingsDigest(merged)
}
