package config

import "testing"

func TestAllowedSettingScopes(t *testing.T) {
	tests := []struct {
		scope string
		name  string
		want  bool
	}{
		{"shared", "agent_id", true},
		{"shared", "limits.active_tasks_per_user", true},
		{"shared", "listen", false},
		{"shared", "public_url", false},
		{"shared", "role", false},
		{"api", "listen", true},
		{"api", "public_url", true},
		{"api", "limits.telegram_timeout", true},
		{"api", "llm.model", false},
		{"worker", "listen", true},
		{"worker", "public_url", false},
		{"worker", "llm.model", true},
		{"worker", "limits.worker_concurrency", true},
		{"worker", "limits.active_tasks_per_user", false},
	}
	for _, test := range tests {
		if got := AllowedSettingScope(test.scope, test.name); got != test.want {
			t.Errorf("AllowedSettingScope(%q, %q) = %t, want %t", test.scope, test.name, got, test.want)
		}
	}
}
