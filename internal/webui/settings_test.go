package webui

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
)

func TestConnectionSettingsNotEditableThroughUI(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "settings.db"))}, ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	settings, err := db.Settings(config.NewSecret(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	base := config.App{Listen: "127.0.0.1:8080", PublicURL: "http://127.0.0.1:8080", Role: "all"}
	server := &Server{Settings: settings, Base: base, Config: base}
	for _, key := range []string{"listen", "public_url", "role"} {
		if err = settings.Put(ctx, key, base.Settings()[key], 0); err == nil {
			t.Fatalf("%s accepted in shared scope", key)
		}
		w := httptest.NewRecorder()
		server.putSetting(w, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"scope":"api","name":"`+key+`","value":"changed","version":1}`)))
		if w.Code != 403 {
			t.Fatalf("%s write status %d", key, w.Code)
		}
		w = httptest.NewRecorder()
		server.deleteSetting(w, httptest.NewRequest("DELETE", "/api/settings", strings.NewReader(`{"scope":"api","name":"`+key+`","version":1}`)))
		if w.Code != 403 {
			t.Fatalf("%s reset status %d", key, w.Code)
		}
	}
	w := httptest.NewRecorder()
	server.settings(w, httptest.NewRequest("GET", "/api/settings", nil))
	for _, key := range []string{"listen", "public_url", "role"} {
		if strings.Contains(w.Body.String(), `"name":"`+key+`"`) {
			t.Fatalf("%s exposed in editor", key)
		}
	}
	values, _, err := settings.Load(ctx)
	if err != nil || len(values) != 0 {
		t.Fatal("protected values changed")
	}
	if !strings.Contains(w.Body.String(), `"name":"memory.interval"`) {
		t.Fatal("ordinary settings disappeared")
	}
	api, _ := settings.InScope("api")
	worker, _ := settings.InScope("worker")
	if err := api.Put(ctx, "log_level", "warn", 0); err != nil {
		t.Fatal(err)
	}
	if err := worker.Put(ctx, "llm.model", "worker-model", 0); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	server.settings(w, httptest.NewRequest("GET", "/api/settings?scope=api", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"log_level"`) || strings.Contains(w.Body.String(), `"name":"llm.model"`) {
		t.Fatal("API settings view crossed scope", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	server.settings(w, httptest.NewRequest("GET", "/api/settings?scope=worker", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"llm.model"`) || !strings.Contains(w.Body.String(), `"override_value":"worker-model"`) {
		t.Fatal("worker settings view lost override", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	server.putSetting(w, httptest.NewRequest("POST", "/api/settings", strings.NewReader(`{"scope":"api","name":"llm.model","value":"wrong","version":0}`)))
	if w.Code != 400 {
		t.Fatalf("cross-scope API write status %d", w.Code)
	}
}
