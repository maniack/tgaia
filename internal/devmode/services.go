package devmode

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/maniack/tgaia/internal/llm"
)

type Model struct{}

type Embedder struct{ Name string }

func (e Embedder) Model() string { return e.Name }
func (e Embedder) Embed(_ context.Context, input string) ([]float32, error) {
	vector := make([]float32, 64)
	for _, word := range strings.Fields(strings.ToLower(input)) {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(word))
		vector[hash.Sum64()%uint64(len(vector))]++
	}
	if strings.TrimSpace(input) == "" {
		return nil, fmt.Errorf("empty embedding input")
	}
	return vector, nil
}

func (Model) Complete(ctx context.Context, m []llm.Message) (string, error) {
	return "Это демонстрационный ответ mock-модели. Для живого ответа отключите llm.mock и запустите LM Studio.", nil
}

// CompleteTools answers with the terminal tool it is offered, so the mock
// exercises the same agent loop as a real model.
func (Model) CompleteTools(ctx context.Context, m []llm.Message, tools []llm.Tool) (llm.Message, error) {
	answer := func(name string, args map[string]any) (llm.Message, error) {
		data, _ := json.Marshal(args)
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "mock", Type: "function", Function: llm.ToolFunction{Name: name, Arguments: string(data)}}}}, nil
	}
	for _, tool := range tools {
		switch tool.Function.Name {
		case "reply":
			return answer("reply", map[string]any{"text": "Это демонстрационный ответ mock-модели. Сообщение прошло через изолированный контекст и DB-очередь. Для живого ответа отключите llm.mock и запустите LM Studio."})
		case "memory_save_summary":
			return answer("memory_save_summary", map[string]any{"summary": "Демонстрационная сводка: в этом чате обсуждали планы и договорённости. Это ответ mock-модели, не LM Studio.", "corrections": []any{}})
		case "image_describe":
			return answer("image_describe", map[string]any{"text": "A simple local-development bot avatar with a friendly, neutral visual style."})
		}
	}
	return llm.Message{Role: "assistant", Content: "mock"}, nil
}

type Sender struct{ next atomic.Int64 }

func (s *Sender) Send(ctx context.Context, chatID int64, text string) (int64, time.Time, error) {
	return time.Now().UnixMicro() + s.next.Add(1), time.Now().UTC(), nil
}
func (*Sender) Edit(ctx context.Context, chatID, messageID int64, text string) error { return nil }
func (*Sender) Delete(ctx context.Context, chatID, messageID int64) error            { return nil }
func Label(mockLLM bool) string {
	if mockLLM {
		return "Local demo · mock OIDC, Telegram and LLM"
	}
	return fmt.Sprint("Local development · mock OIDC / Telegram; configured LLM")
}
