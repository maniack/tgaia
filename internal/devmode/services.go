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
	if strings.Contains(m[0].Content, "cumulative long-term memory") {
		return "Демонстрационная сводка: в этом чате обсуждали планы и договорённости. Это ответ mock-модели, не LM Studio.", nil
	}
	reply := "Это демонстрационный ответ mock-модели. Сообщение прошло через изолированный контекст и DB-очередь. Для живого ответа отключите llm.mock и запустите LM Studio."
	data, _ := json.Marshal(map[string]any{"reply": reply})
	return string(data), nil
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
