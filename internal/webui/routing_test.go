package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maniack/tgaia/internal/config"
)

func TestDedicatedWebhookRouteIsolation(t *testing.T) {
	cfg := config.App{TelegramMode: "webhook", TelegramWebhookListen: "127.0.0.1:8081"}
	server := &Server{Config: cfg}

	request := func(handler http.Handler, method, path string) int {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
		return response.Code
	}

	if status := request(server.Handler(), http.MethodPost, "/telegram/1"); status != http.StatusNotFound {
		t.Fatalf("webhook exposed on application listener: %d", status)
	}
	if status := request(server.WebhookHandler(), http.MethodPost, "/telegram/1"); status != http.StatusUnauthorized {
		t.Fatalf("webhook missing from dedicated listener: %d", status)
	}
	if status := request(server.WebhookHandler(), http.MethodGet, "/login"); status != http.StatusNotFound {
		t.Fatalf("application route exposed on webhook listener: %d", status)
	}

	cfg.TelegramWebhookListen = ""
	if status := request((&Server{Config: cfg}).Handler(), http.MethodPost, "/telegram/1"); status != http.StatusUnauthorized {
		t.Fatalf("default webhook route missing from application listener: %d", status)
	}
}

func TestApplicationResponsesCarryRequestID(t *testing.T) {
	response := httptest.NewRecorder()
	(&Server{Config: config.App{Role: "worker"}}).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/missing", nil))
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("response is missing request ID")
	}
}
