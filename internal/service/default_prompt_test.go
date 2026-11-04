package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPromptFallbackAndFileRefresh(t *testing.T) {
	e, model, _, scope := engineFixture(t)
	ctx := context.Background()
	for i, prompt := range []string{"Fallback first revision", "Fallback second revision"} {
		if err := os.WriteFile(e.DefaultPromptFile, []byte(prompt), 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.DB.Bots().SetPrompt(ctx, scope.AgentID, scope.BotID, " \n\t"); err != nil {
			t.Fatal(err)
		}
		accept(t, e, scope, int64(i+1), "hello", true)
		job := take(t, e, "respond")
		if err := e.Respond(ctx, *job); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(model.input[0].Content, prompt) {
			t.Fatal("default prompt not read for this job")
		}
		if err := e.DB.Queue().Complete(ctx, job.Receipt); err != nil {
			t.Fatal(err)
		}
	}
	// A per-bot override must work even if the fallback file is unavailable.
	if err := e.DB.Bots().SetPrompt(ctx, scope.AgentID, scope.BotID, "Individual bot prompt"); err != nil {
		t.Fatal(err)
	}
	e.DefaultPromptFile = filepath.Join(t.TempDir(), "missing.md")
	accept(t, e, scope, 3, "hello", true)
	job := take(t, e, "respond")
	if err := e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(model.input[0].Content, "Individual bot prompt") || strings.Contains(model.input[0].Content, "Fallback") {
		t.Fatal("per-bot prompt did not override fallback")
	}
}
func TestDefaultPromptFileValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system-prompt.md")
	if _, err := readDefaultPrompt(path); err == nil {
		t.Fatal("missing file accepted")
	}
	for _, content := range []string{" \n", strings.Repeat("x", 16001)} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDefaultPrompt(path); err == nil {
			t.Fatal("invalid prompt accepted")
		}
	}
}
