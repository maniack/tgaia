package config

import (
	"errors"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type App struct {
	Limits                map[string]string
	OTLPEndpoint          string
	ObservabilityListen   string
	DefaultPromptFile     string
	MCPServers            []MCPServer
	LogLevel              slog.Level
	AgentID               string
	Listen                string
	PublicURL             string
	Role                  string
	LLMEndpoint           string
	LLMModel              string
	LLMKey                Secret
	MockLLM               bool
	OIDCMode              string
	OIDCIssuer            string
	OIDCClientID          string
	OIDCClientSecret      Secret
	SessionKey            Secret
	AdminGroups           []string
	TelegramMode          string
	TelegramWebhookListen string
	Bots                  []Bot
	MemoryWindow          time.Duration
	MemoryInterval        time.Duration
}
type Bot struct {
	DisplayName   string // Resolved by getMe; not a configuration setting.
	Name          string
	ID            int64
	Username      string
	Token         Secret
	WebhookSecret Secret
}
type appSchema struct {
	Limits        map[string]string `yaml:"limits"`
	Observability struct {
		OTLPEndpoint string `yaml:"otlp_endpoint"`
		Listen       string `yaml:"listen"`
	} `yaml:"observability"`
	MCP struct {
		Servers map[string]mcpServerSchema `yaml:"servers"`
	} `yaml:"mcp"`
	AgentID   string `yaml:"agent_id"`
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
	Role      string `yaml:"role"`
	LLM       struct {
		Endpoint string `yaml:"endpoint"`
		Model    string `yaml:"model"`
		APIKey   string `yaml:"api_key"`
		Mock     string `yaml:"mock"`
	} `yaml:"llm"`
	OIDC struct {
		Mode         string `yaml:"mode"`
		Issuer       string `yaml:"issuer"`
		ClientID     string `yaml:"client_id"`
		ClientSecret string `yaml:"client_secret"`
		SessionKey   string `yaml:"session_key"`
		AdminGroups  string `yaml:"admin_groups"`
	} `yaml:"oidc"`
	Telegram struct {
		Mode          string `yaml:"mode"`
		WebhookListen string `yaml:"webhook_listen"`
		Bots          map[string]struct {
			ID            string `yaml:"id"`
			Username      string `yaml:"username"`
			Token         string `yaml:"token"`
			WebhookSecret string `yaml:"webhook_secret"`
		} `yaml:"bots"`
	} `yaml:"telegram"`
	Memory struct {
		Window   string `yaml:"window"`
		Interval string `yaml:"interval"`
	} `yaml:"memory"`
}

func (f *File) App() (App, error) {
	if err := f.Load(); err != nil {
		return App{}, err
	}
	get := func(key, env, fallback string) string {
		if v, ok := os.LookupEnv(env); ok && env != "" {
			return v
		}
		if v, ok := f.Value("app." + key); ok {
			return v
		}
		return fallback
	}
	c := App{AgentID: get("agent_id", "TGAIA_AGENT_ID", ""), Listen: get("listen", "TGAIA_LISTEN", ""), PublicURL: get("public_url", "TGAIA_PUBLIC_URL", ""), Role: get("role", "TGAIA_ROLE", ""), LLMEndpoint: get("llm.endpoint", "TGAIA_LLM_ENDPOINT", ""), LLMModel: get("llm.model", "TGAIA_LLM_MODEL", ""), LLMKey: NewSecret(get("llm.api_key", "TGAIA_LLM_API_KEY", "")), OIDCMode: get("oidc.mode", "TGAIA_OIDC_MODE", "oidc"), OIDCIssuer: get("oidc.issuer", "TGAIA_OIDC_ISSUER", ""), OIDCClientID: get("oidc.client_id", "TGAIA_OIDC_CLIENT_ID", ""), OIDCClientSecret: NewSecret(get("oidc.client_secret", "TGAIA_OIDC_CLIENT_SECRET", "")), SessionKey: NewSecret(get("oidc.session_key", "TGAIA_SESSION_KEY", "")), AdminGroups: splitCSV(get("oidc.admin_groups", "TGAIA_ADMIN_GROUPS", "")), TelegramMode: get("telegram.mode", "TGAIA_TELEGRAM_MODE", "webhook")}
	c.OTLPEndpoint = get("observability.otlp_endpoint", "TGAIA_OTLP_ENDPOINT", "")
	c.ObservabilityListen = get("observability.listen", "TGAIA_OBSERVABILITY_LISTEN", "127.0.0.1:9090")
	c.TelegramWebhookListen = get("telegram.webhook_listen", "TGAIA_TELEGRAM_WEBHOOK_LISTEN", "")
	var err error
	c.MockLLM, err = strconv.ParseBool(get("llm.mock", "TGAIA_LLM_MOCK", "false"))
	if err != nil {
		return c, errors.New("invalid llm.mock")
	}
	c.MemoryWindow, err = time.ParseDuration(get("memory.window", "TGAIA_MEMORY_WINDOW", "1h"))
	if err != nil {
		return c, errors.New("invalid memory.window")
	}
	c.MemoryInterval, err = time.ParseDuration(get("memory.interval", "TGAIA_MEMORY_INTERVAL", "30s"))
	if err != nil {
		return c, errors.New("invalid memory.interval")
	}
	if f.app != nil {
		c.Limits = f.app.Limits
		c.MCPServers, err = parseMCPFile(f.app.MCP.Servers)
		if err != nil {
			return c, err
		}
		for name, b := range f.app.Telegram.Bots {
			id := int64(0)
			if b.ID != "" {
				id, err = strconv.ParseInt(b.ID, 10, 64)
				if err != nil {
					return c, errors.New("invalid bot id")
				}
			}
			c.Bots = append(c.Bots, Bot{Name: name, ID: id, Username: b.Username, Token: NewSecret(b.Token), WebhookSecret: NewSecret(b.WebhookSecret)})
		}
	}
	c.DefaultPromptFile, err = filepath.Abs(filepath.Join(filepath.Dir(f.Path), "system-prompt.md"))
	if err != nil {
		return c, errors.New("cannot resolve default system prompt path")
	}
	return c, c.Validate()
}
func (c App) Validate() error {
	if err := c.validateLimits(); err != nil {
		return err
	}
	if c.OTLPEndpoint != "" {
		u, err := url.Parse(c.OTLPEndpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("invalid OTLP endpoint URL")
		}
	}

	if !c.MockLLM && (c.LLMEndpoint == "" || c.LLMModel == "") {
		return errors.New("LLM endpoint and model must be explicitly configured")
	}
	if c.OIDCClientID == "" {
		return errors.New("OIDC client ID must be explicitly configured")
	}
	if c.AgentID == "" || len(c.AgentID) > 128 || len(c.Bots) == 0 {
		return errors.New("agent and at least one bot are required")
	}
	if c.Role != "all" && c.Role != "api" && c.Role != "worker" {
		return errors.New("role must be all, api or worker")
	}
	if c.TelegramMode != "mock" && c.TelegramMode != "polling" && c.TelegramMode != "webhook" {
		return errors.New("telegram mode must be mock, polling or webhook")
	}
	if c.OIDCMode != "mock" && c.OIDCMode != "oidc" {
		return errors.New("OIDC mode must be mock or oidc")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("invalid listen address")
	}
	observabilityListen := c.ObservabilityAddress()
	observabilityHost, observabilityPort, err := net.SplitHostPort(observabilityListen)
	if err != nil || observabilityPort == port {
		return errors.New("invalid or conflicting observability listen address")
	}
	webhookHost := ""
	if c.TelegramWebhookListen != "" {
		var webhookPort string
		webhookHost, webhookPort, err = net.SplitHostPort(c.TelegramWebhookListen)
		if err != nil || webhookPort == port || webhookPort == observabilityPort {
			return errors.New("invalid or conflicting Telegram webhook listen address")
		}
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid public URL")
	}
	local := host == "127.0.0.1" || host == "::1" || host == "localhost"
	observabilityLocal := observabilityHost == "127.0.0.1" || observabilityHost == "::1" || observabilityHost == "localhost"
	webhookLocal := c.TelegramWebhookListen == "" || webhookHost == "127.0.0.1" || webhookHost == "::1" || webhookHost == "localhost"
	if c.OIDCMode == "mock" || c.TelegramMode == "mock" || c.MockLLM {
		if !local || !observabilityLocal || !webhookLocal || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
			return errors.New("mocks are permitted only on loopback")
		}
	}
	if c.OIDCMode == "oidc" && (c.OIDCIssuer == "" || len(c.SessionKey.Reveal()) < 32 || len(c.AdminGroups) == 0) {
		return errors.New("OIDC issuer, session key and admin groups required")
	}
	if c.MemoryWindow < time.Second || c.MemoryInterval < time.Second {
		return errors.New("memory durations must be at least 1s")
	}
	for _, bot := range c.Bots {
		if c.TelegramMode == "mock" {
			if bot.ID <= 0 || bot.Username == "" {
				return errors.New("mock bots require id and username")
			}
		} else {
			if bot.Token.Reveal() == "" {
				return errors.New("Telegram token required")
			}
			if c.TelegramMode == "webhook" && len(bot.WebhookSecret.Reveal()) < 16 {
				return errors.New("webhook secret must contain at least 16 characters")
			}
		}
	}
	return c.validateMCP()
}

func (c App) ObservabilityAddress() string {
	if c.ObservabilityListen == "" {
		return "127.0.0.1:9090"
	}
	return c.ObservabilityListen
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
