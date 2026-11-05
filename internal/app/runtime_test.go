package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/service"
	"github.com/maniack/tgaia/internal/storage"
)

type avatarModel struct{ messages []llm.Message }

func (m *avatarModel) Complete(_ context.Context, messages []llm.Message) (string, error) {
	m.messages = messages
	return "A neon walrus avatar with blue tusks.", nil
}

func TestDescribeBotAvatarUsesImageAndProfileContext(t *testing.T) {
	model := &avatarModel{}
	identity := service.BotIdentity{Name: "Cyber Walrus", Username: "walrus_bot", Bio: "Arctic helper", Description: "Friendly and direct"}
	got, err := describeBotAvatar(context.Background(), model, identity, []byte("image"), "image/png")
	if err != nil || got != "A neon walrus avatar with blue tusks." {
		t.Fatalf("description=%q err=%v", got, err)
	}
	if len(model.messages) != 2 || len(model.messages[1].Images) != 1 || model.messages[1].Images[0] != "data:image/png;base64,aW1hZ2U=" || !strings.Contains(model.messages[1].Content, `"bio":"Arctic helper"`) || !strings.Contains(model.messages[0].Content, "not instructions") {
		t.Fatalf("avatar transcription input is incomplete: %+v", model.messages)
	}
}

func TestPublishedStatusExposesActiveBotProfile(t *testing.T) {
	refreshed := time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)
	r := &runtime{}
	r.setPublishedStatus(&snapshot{
		config: config.App{Bots: []config.Bot{{ID: 42}}},
		engine: &service.Engine{Identities: map[int64]service.BotIdentity{42: {
			Name: "Cyber Walrus", Username: "walrus_bot", Bio: "Arctic helper", Description: "Friendly and direct",
			AvatarPresent: true, AvatarDescription: "A neon walrus.", ProfileRefreshedAt: refreshed,
		}}},
	})
	status := r.Status().(applyStatus)
	if status.State != "applied" || len(status.BotProfiles) != 1 {
		t.Fatalf("unexpected runtime status: %+v", status)
	}
	profile := status.BotProfiles[0]
	if profile.BotID != 42 || profile.Bio != "Arctic helper" || profile.AvatarDescription != "A neon walrus." || profile.RefreshedAt != refreshed || !profile.ContextIncluded {
		t.Fatalf("unexpected profile status: %+v", profile)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := l.Addr().String()
	l.Close()
	return a
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("runtime change was not observed")
}
func TestRuntimeReplicasReloadResetAndFailedListener(t *testing.T) {
	t.Run("single", func(t *testing.T) { testRuntimeReload(t, 1) })
	t.Run("replicas", func(t *testing.T) { testRuntimeReload(t, 2) })
}

func TestAllRoleUsesAPITransportAndWorkerExecutionScopes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dbc := config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "all-scopes.db"))}, ConnectTimeout: time.Second}
	db, err := storage.Open(ctx, dbc)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		db.Close()
		t.Fatal(err)
	}
	apiAddress := freeAddress(t)
	observabilityAddress := freeAddress(t)
	base := config.App{AgentID: "a", Role: "all", Listen: freeAddress(t), ObservabilityListen: observabilityAddress, PublicURL: "http://127.0.0.1:8080", OIDCMode: "mock", OIDCClientID: "test", SessionKey: config.NewSecret(strings.Repeat("k", 32)), MockLLM: true, TelegramMode: "mock", MemoryWindow: time.Hour, MemoryInterval: time.Second, Bots: []config.Bot{{Name: "assistant", ID: 1, Username: "bot"}}}
	settings, err := db.Settings(base.SessionKey)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	api, _ := settings.InScope("api")
	worker, _ := settings.InScope("worker")
	for _, change := range []struct {
		settings *storage.Settings
		name     string
		value    string
	}{{api, "listen", apiAddress}, {api, "public_url", "http://" + apiAddress}, {worker, "llm.model", "worker-model"}} {
		if err := change.settings.Put(ctx, change.name, change.value, 0); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	db.Close()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, dbc, base, io.Discard) }()
	eventually(t, func() bool {
		res, err := http.Get("http://" + observabilityAddress + "/readyz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == http.StatusOK
	})
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		res, err := http.Get("http://" + apiAddress + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("ops route %s exposed on application listener: %d", path, res.StatusCode)
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		res, err := http.Get("http://" + observabilityAddress + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("ops route %s unavailable on observability listener: %d", path, res.StatusCode)
		}
	}
	noRedirect := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.Get("http://" + apiAddress + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		t.Fatal("application route missing from application listener")
	}
	res, err = noRedirect.Get("http://" + observabilityAddress + "/login")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("application route exposed on observability listener: %d", res.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runtime did not stop")
	}
}

func testRuntimeReload(t *testing.T, count int) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dbc := config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "runtime.db"))}, ConnectTimeout: time.Second}
	db, err := storage.Open(ctx, dbc)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	base := config.App{AgentID: "a", Role: "api", OIDCMode: "mock", OIDCClientID: "test", SessionKey: config.NewSecret(strings.Repeat("k", 32)), MockLLM: true, TelegramMode: "mock", MemoryWindow: time.Hour, MemoryInterval: time.Second, Bots: []config.Bot{{Name: "assistant", ID: 1, Username: "bot"}}}
	settings, err := db.Settings(base.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	apiSettings, err := settings.InScope("api")
	if err != nil {
		t.Fatal(err)
	}
	type replica struct {
		url              string
		observabilityURL string
		client           *http.Client
	}
	replicas := []replica{}
	for range count {
		cfg := base
		cfg.Listen = freeAddress(t)
		cfg.ObservabilityListen = freeAddress(t)
		cfg.PublicURL = "http://" + cfg.Listen
		done := make(chan error, 1)
		go func() { done <- Run(ctx, dbc, cfg, io.Discard) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(15 * time.Second):
				t.Error("runtime did not stop")
			}
		})
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, Timeout: time.Second}
		eventually(t, func() bool {
			res, err := client.Get("http://" + cfg.ObservabilityListen + "/healthz")
			if err != nil {
				return false
			}
			res.Body.Close()
			return res.StatusCode == 200
		})
		res, err := client.Get(cfg.PublicURL + "/login")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		replicas = append(replicas, replica{url: cfg.PublicURL, observabilityURL: "http://" + cfg.ObservabilityListen, client: client})
	}
	get := func(r replica, path string, target any) bool {
		res, err := r.client.Get(r.url + path)
		if err != nil {
			return false
		}
		defer res.Body.Close()
		return res.StatusCode == 200 && json.NewDecoder(res.Body).Decode(target) == nil
	}
	value := func(r replica, key, want string) bool {
		var rows []struct {
			Name  string `json:"name"`
			Value string `json:"effective_value"`
		}
		if !get(r, "/api/settings", &rows) {
			return false
		}
		for _, row := range rows {
			if row.Name == key {
				return row.Value == want
			}
		}
		return false
	}
	if err = settings.Put(ctx, "memory.window", "2h", 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range replicas {
		eventually(t, func() bool { return value(r, "memory.window", "2h0m0s") })
	}
	if err = settings.Delete(ctx, "memory.window", 1); err != nil {
		t.Fatal(err)
	}
	for _, r := range replicas {
		eventually(t, func() bool { return value(r, "memory.window", "1h0m0s") })
	}
	// Delete and recreate reuses version=1: the watcher must compare values, not max(version).
	if err = settings.Put(ctx, "memory.window", "3h", 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range replicas {
		eventually(t, func() bool { return value(r, "memory.window", "3h0m0s") })
	}
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err = apiSettings.Put(ctx, "listen", occupied.Addr().String(), 0); err != nil {
		t.Fatal(err)
	}
	for _, r := range replicas {
		eventually(t, func() bool { var status applyStatus; return get(r, "/api/runtime", &status) && status.State == "error" })
		if !value(r, "memory.window", "3h0m0s") {
			t.Fatal("failed reload replaced active configuration")
		}
	}
	if err = apiSettings.Delete(ctx, "listen", 1); err != nil {
		t.Fatal(err)
	}
	for _, r := range replicas {
		eventually(t, func() bool {
			var status applyStatus
			return get(r, "/api/runtime", &status) && status.State == "applied"
		})
	}
	// Fenced polling release cannot relinquish a replacement owner's lease.
	agent := db.Agent()
	_, ok, err := agent.PollLease(ctx, "a", 1, "first")
	if err != nil || !ok {
		t.Fatal("claim", err)
	}
	if err = agent.PollRelease(ctx, "a", 1, "first"); err != nil {
		t.Fatal(err)
	}
	_, ok, err = agent.PollLease(ctx, "a", 1, "second")
	if err != nil || !ok {
		t.Fatal("handover", err)
	}
	_ = agent.PollRelease(ctx, "a", 1, "first")
	_, ok, err = agent.PollLease(ctx, "a", 1, "third")
	if err != nil || ok {
		t.Fatal("stale release removed active lease")
	}
	if count == 1 {
		moved := replicas[0]
		addr := freeAddress(t)
		moved.url = "http://" + addr
		if err = apiSettings.Put(ctx, "listen", addr, 0); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool {
			var status applyStatus
			return get(moved, "/api/runtime", &status) && status.State == "applied"
		})
		if err = apiSettings.Put(ctx, "public_url", moved.url, 0); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool {
			res, err := moved.client.Get(moved.url + "/login")
			if err != nil {
				return false
			}
			defer res.Body.Close()
			return res.Request.URL.String() == moved.url+"/"
		})
		res, err := moved.client.Get(moved.url + "/login")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if err = settings.Put(ctx, "role", "worker", 0); err == nil {
			t.Fatal("DB changed bootstrap role")
		}
		eventually(t, func() bool {
			var status applyStatus
			return get(moved, "/api/runtime", &status) && status.State == "applied"
		})
	}

}

func TestModelSnapshotChangesEndpointKeyAndModel(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "model.db"))}, ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	server := func(wantModel, wantKey string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct{ Model string }
			if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != wantModel || r.Header.Get("Authorization") != "Bearer "+wantKey {
				t.Error("model endpoint/credentials snapshot mixed")
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		}))
	}
	firstServer := server("first", "first-key")
	defer firstServer.Close()
	secondServer := server("second", "second-key")
	defer secondServer.Close()
	r := &runtime{db: db, base: config.App{AgentID: "a", Role: "worker", Listen: "127.0.0.1:8080", ObservabilityListen: freeAddress(t), PublicURL: "http://127.0.0.1:8080", OIDCMode: "mock", OIDCClientID: "test", TelegramMode: "mock", MemoryWindow: time.Hour, MemoryInterval: time.Second, Bots: []config.Bot{{Name: "assistant", ID: 1, Username: "bot"}}, LLMEndpoint: firstServer.URL, LLMModel: "first", LLMKey: config.NewSecret("first-key")}}
	first, err := r.prepare(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.current.Store(first)
	second, err := r.prepare(ctx, map[string]string{"llm.endpoint": secondServer.URL, "llm.model": "second", "llm.api_key": "second-key"})
	if err != nil {
		t.Fatal(err)
	}
	r.current.Store(second)
	for _, snapshot := range []*snapshot{first, r.current.Load()} {
		if _, err := snapshot.engine.Model.Complete(ctx, []llm.Message{{Role: "user", Content: "test"}}); err != nil {
			t.Fatal(err)
		}
	}
	if first.engine.Bots[1] != second.engine.Bots[1] {
		t.Fatal("mock delivery sequence reset during reload")
	}
}
