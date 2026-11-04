package config

import (
	"errors"
	"github.com/maniack/tgaia/internal/queue"
	"strconv"
	"strings"
	"time"
)

type limitSpec struct {
	Default  string
	Min, Max int
	Duration bool
}

var limitSpecs = map[string]limitSpec{
	"mcp_call_timeout": {"20s", 1000, 3600000, true}, "mcp_result_bytes": {"16000", 1000, 30000, false},
	"telegram_timeout": {"35s", 31000, 300000, true}, "image_bytes": {"8388608", 1024, 8388608, false}, "document_bytes": {"262144", 1024, 1048576, false},
	"worker_concurrency": {"4", 1, 256, false},
	"worker_poll":        {"1s", 1, 60000, true}, "worker_lease": {"30s", 3000, 3600000, true}, "worker_heartbeat": {"10s", 100, 1200000, true},
	"worker_timeout": {"12m", 1000, 7200000, true}, "worker_shutdown": {"15s", 1000, 300000, true}, "retry_base": {"1s", 1, 60000, true}, "retry_max": {"1m", 1, 3600000, true},
	"llm_concurrency": {"4", 1, 256, false}, "llm_timeout": {"5m", 1000, 3600000, true}, "llm_first_token_timeout": {"2m", 1000, 3600000, true}, "llm_idle_timeout": {"45s", 1000, 3600000, true},
	"llm_input_bytes": {"128000", 16000, 1000000, false}, "llm_output_tokens": {"2048", 64, 65536, false}, "embedding_timeout": {"30s", 1000, 3600000, true},
	"reply_messages": {"30", 1, 200, false}, "reply_context_bytes": {"100000", 1000, 900000, false},
	"compaction_messages": {"100", 1, 200, false}, "compaction_input_bytes": {"90000", 1000, 900000, false}, "compaction_output_tokens": {"2048", 64, 65536, false},
	"tool_timeout": {"5m", 1000, 3600000, true}, "tool_rounds": {"4", 1, 32, false}, "tool_calls": {"8", 1, 64, false}, "tool_context_bytes": {"118000", 1000, 950000, false},
	"media_messages":          {"4", 1, 20, false},
	"llm_calls_per_chat_hour": {"0", 0, 100000, false}, "compaction_calls_per_chat_hour": {"0", 0, 100000, false}, "active_tasks_per_user": {"0", 0, 10000, false},
}

func (c App) Limit(name string) int {
	v := c.Limits[name]
	spec := limitSpecs[name]
	if v == "" {
		v = spec.Default
	}
	if spec.Duration {
		d, _ := time.ParseDuration(v)
		return int(d.Milliseconds())
	}
	n, _ := strconv.Atoi(v)
	return n
}
func (c App) Duration(name string) time.Duration {
	return time.Duration(c.Limit(name)) * time.Millisecond
}
func (c App) validateLimits() error {
	for name, v := range c.Limits {
		spec, ok := limitSpecs[name]
		if !ok || strings.TrimSpace(v) == "" {
			return errors.New("unknown or empty operational limit")
		}
		n := 0
		var err error
		if spec.Duration {
			var d time.Duration
			d, err = time.ParseDuration(v)
			n = int(d.Milliseconds())
		} else {
			n, err = strconv.Atoi(v)
		}
		if err != nil || n < spec.Min || n > spec.Max {
			return errors.New("operational limit outside supported range: " + name)
		}
	}
	if c.Duration("worker_heartbeat") > c.Duration("worker_lease")/3 || c.Duration("retry_base") > c.Duration("retry_max") {
		return errors.New("invalid worker heartbeat/retry limits")
	}
	for _, name := range []string{"reply_context_bytes", "compaction_input_bytes", "tool_context_bytes"} {
		if c.Limit(name) >= c.Limit("llm_input_bytes") {
			return errors.New("context budget must be below LLM input limit")
		}
	}
	// A job attempt spans the tool phase plus the final model call. A shorter
	// worker_timeout would silently cancel every long response.
	if c.Duration("worker_timeout") <= c.Duration("tool_timeout")+c.Duration("llm_timeout") {
		return errors.New("worker_timeout must exceed tool_timeout plus llm_timeout")
	}
	if c.Duration("llm_first_token_timeout") > c.Duration("llm_timeout") || c.Duration("llm_idle_timeout") > c.Duration("llm_timeout") {
		return errors.New("llm_first_token_timeout and llm_idle_timeout must not exceed llm_timeout")
	}
	return nil
}
func (c App) WorkerOptions() queue.Options {
	return queue.Options{Concurrency: c.Limit("worker_concurrency"), PollInterval: c.Duration("worker_poll"), Lease: c.Duration("worker_lease"), Heartbeat: c.Duration("worker_heartbeat"), TaskTimeout: c.Duration("worker_timeout"), ShutdownTimeout: c.Duration("worker_shutdown"), RetryBase: c.Duration("retry_base"), RetryMax: c.Duration("retry_max")}
}
