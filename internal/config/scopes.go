package config

import "strings"

// Identity and cross-role policies remain shared; transport/compute knobs may specialize.
func AllowedSettingScope(scope, name string) bool {
	if scope == "" || scope == "shared" {
		return name != "role" && name != "listen" && name != "public_url"
	}
	if name == "log_level" || name == "observability.otlp_endpoint" {
		return scope == "api" || scope == "worker"
	}
	if scope == "api" {
		return name == "listen" || name == "public_url" || name == "limits.telegram_timeout"
	}
	if scope == "worker" {
		return name == "listen" || strings.HasPrefix(name, "llm.") || strings.HasPrefix(name, "memory.") || name == "mcp.servers" || (strings.HasPrefix(name, "limits.") && name != "limits.active_tasks_per_user")
	}
	return false
}
