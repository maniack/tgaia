package config

import (
	"testing"
	"time"
)

func TestOperationalLimits(t *testing.T) {
	for _, values := range []map[string]string{{"worker_concurrency": "0"}, {"worker_heartbeat": "20s"}, {"retry_base": "2m"}, {"compaction_messages": "201"}, {"llm_calls_per_chat_hour": "-1"}, {"unknown": "3"}, {"worker_timeout": "45s"}, {"llm_idle_timeout": "6m"}, {"llm_first_token_timeout": "6m"}} {
		if (App{Limits: values}).validateLimits() == nil {
			t.Fatal("invalid limits accepted", values)
		}
	}
	if (App{}).validateLimits() != nil {
		t.Fatal("defaults must be consistent")
	}
	c := App{Limits: map[string]string{"worker_concurrency": "2", "compaction_messages": "12", "worker_timeout": "45s", "tool_timeout": "20s", "llm_timeout": "20s", "llm_first_token_timeout": "10s", "llm_idle_timeout": "10s"}}
	if err := c.validateLimits(); err != nil {
		t.Fatal(err)
	}
	if c.WorkerOptions().Concurrency != 2 || c.WorkerOptions().TaskTimeout != 45*time.Second || c.Limit("compaction_messages") != 12 {
		t.Fatal("limits not applied")
	}
}
