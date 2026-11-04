package devmode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/maniack/tgaia/internal/llm"
)

type Model struct{}

func (Model) Complete(ctx context.Context, m []llm.Message) (string, error) {
	if strings.Contains(m[0].Content, "Summarize") {
		return "Демонстрационная сводка: в этом чате обсуждали планы и договорённости. Это ответ mock-модели, не LM Studio.", nil
	}
	reply := "Это демонстрационный ответ mock-модели. Сообщение прошло через изолированный контекст и DB-очередь. Для живого ответа отключите llm.mock и запустите LM Studio."
	if len(m) > 1 && strings.Contains(strings.ToLower(m[len(m)-1].Content), "напомни") {
		data, _ := json.Marshal(map[string]any{"reply": "Демонстрационная задача создана на следующую минуту.", "schedule": map[string]any{"prompt": "Напомни о тестовой проверке", "delay_seconds": 60, "interval_seconds": 0}})
		return string(data), nil
	}
	data, _ := json.Marshal(map[string]any{"reply": reply, "schedule": nil})
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
