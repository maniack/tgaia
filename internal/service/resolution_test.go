package service

import (
	"context"
	"github.com/maniack/tgaia/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestResolvedMultipartDoesNotRepeatUncertainPart(t *testing.T) {
	e, model, sender, sc := engineFixture(t)
	ctx := context.Background()
	model.reply = `{"reply":"` + strings.Repeat("a", 4000) + `","schedule":null}`
	accept(t, e, sc, 1, "test", true)
	job := take(t, e, "respond")
	sender.fail = true
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.DB.Agent().ResolveOutcome(ctx, storage.Resolution{JobID: job.ID, Target: "delivery:0", Version: 2, Action: "confirm", MessageID: 900, SentAt: time.Now().UnixMilli(), Reason: "Checked externally"}, "admin"); err != nil {
		t.Fatal(err)
	}
	job = take(t, e, "respond")
	sender.fail = false
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if sender.calls != 2 || model.calls != 1 {
		t.Fatalf("replayed send/model: sends %d model %d", sender.calls, model.calls)
	}
}
