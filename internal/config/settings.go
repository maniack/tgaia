package config

import (
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

func (c App) Settings() map[string]string {
	values := map[string]string{"observability.otlp_endpoint": c.OTLPEndpoint, "mcp.servers": EncodeMCP(c.MCPServers), "log_level": strings.ToLower(c.LogLevel.String()), "agent_id": c.AgentID, "listen": c.Listen, "public_url": c.PublicURL, "role": c.Role, "llm.endpoint": c.LLMEndpoint, "llm.model": c.LLMModel, "llm.api_key": c.LLMKey.Reveal(), "llm.mock": strconv.FormatBool(c.MockLLM), "llm.thinking": strconv.FormatBool(c.LLMThinking), "telegram.mode": c.TelegramMode, "memory.embedding_model": c.EmbeddingModel, "memory.window": c.MemoryWindow.String(), "memory.interval": c.MemoryInterval.String()}
	for name, spec := range limitSpecs {
		value := c.Limits[name]
		if value == "" {
			value = spec.Default
		}
		values["limits."+name] = value
	}
	for _, b := range c.Bots {
		prefix := fmt.Sprintf("bots.%s.", b.Name)
		values[prefix+"id"] = strconv.FormatInt(b.ID, 10)
		values[prefix+"username"] = b.Username
		values[prefix+"token"] = b.Token.Reveal()
		values[prefix+"webhook_secret"] = b.WebhookSecret.Reveal()
	}
	return values
}
func SecretSetting(key string) bool {
	return key == "mcp.servers" || key == "llm.api_key" || strings.HasSuffix(key, ".token") || strings.HasSuffix(key, ".webhook_secret")
}
func (c App) WithSettings(values map[string]string) (App, error) {
	c.Limits = maps.Clone(c.Limits)
	if c.Limits == nil {
		c.Limits = map[string]string{}
	}
	c.Bots = append([]Bot(nil), c.Bots...)
	c.MCPServers = append([]MCPServer(nil), c.MCPServers...)
	allowed := c.Settings()
	for key, value := range values {
		if _, ok := allowed[key]; !ok {
			return c, errors.New("unknown setting or bootstrap-only setting")
		}
		if strings.HasPrefix(key, "limits.") {
			c.Limits[strings.TrimPrefix(key, "limits.")] = value
			continue
		}
		var err error
		switch key {
		case "observability.otlp_endpoint":
			c.OTLPEndpoint = value
		case "mcp.servers":
			c.MCPServers, err = DecodeMCP(value)
		case "log_level":
			if value != "debug" && value != "info" && value != "warn" && value != "error" {
				return c, errors.New("invalid log level")
			}
			err = c.LogLevel.UnmarshalText([]byte(value))
		case "agent_id":
			c.AgentID = value
		case "listen":
			c.Listen = value
		case "public_url":
			c.PublicURL = value
		case "role":
			c.Role = value
		case "llm.endpoint":
			c.LLMEndpoint = value
		case "llm.model":
			c.LLMModel = value
		case "llm.api_key":
			c.LLMKey = NewSecret(value)
		case "llm.mock":
			c.MockLLM, err = strconv.ParseBool(value)
		case "llm.thinking":
			c.LLMThinking, err = strconv.ParseBool(value)
		case "telegram.mode":
			c.TelegramMode = value
		case "memory.window":
			c.MemoryWindow, err = time.ParseDuration(value)
		case "memory.interval":
			c.MemoryInterval, err = time.ParseDuration(value)
		case "memory.embedding_model":
			c.EmbeddingModel = value
		default:
			for i := range c.Bots {
				b := &c.Bots[i]
				prefix := "bots." + b.Name + "."
				switch key {
				case prefix + "id":
					b.ID, err = strconv.ParseInt(value, 10, 64)
				case prefix + "username":
					b.Username = value
				case prefix + "token":
					b.Token = NewSecret(value)
				case prefix + "webhook_secret":
					b.WebhookSecret = NewSecret(value)
				}
			}
		}
		if err != nil {
			return c, errors.New("invalid setting value")
		}
	}
	return c, c.Validate()
}
