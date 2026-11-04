package service

import (
	"context"
	"errors"
	"github.com/maniack/tgaia/internal/storage"
	"testing"
)

func TestModelQuotaStopsProvider(t *testing.T) {
	e, m, _, sc := engineFixture(t)
	ctx := context.Background()
	e.Config.Limits = map[string]string{"llm_calls_per_chat_hour": "1"}
	accept(t, e, sc, 1, "first", true)
	j := take(t, e, "respond")
	if err := e.Respond(ctx, *j); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, j.Receipt); err != nil {
		t.Fatal(err)
	}
	accept(t, e, sc, 2, "second", true)
	j = take(t, e, "respond")
	if err := e.Respond(ctx, *j); !errors.Is(err, storage.ErrQuota) {
		t.Fatal("quota not enforced", err)
	}
	if m.calls != 1 {
		t.Fatal("denied request reached provider")
	}
}
