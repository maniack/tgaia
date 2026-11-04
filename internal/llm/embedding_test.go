package llm

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/maniack/tgaia/internal/config"
)

func TestEmbeddingClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("unexpected embedding request: %s %#v", r.URL.Path, r.Header)
		}
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "bge-m3" || body.Input != "hello" {
			t.Fatalf("unexpected embedding body: %+v", body)
		}
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[3,4]}]}`))
	}))
	defer server.Close()
	client, err := NewEmbedder(server.URL, "bge-m3", config.NewSecret("secret"), config.App{})
	if err != nil {
		t.Fatal(err)
	}
	vector, err := client.Embed(t.Context(), "hello")
	if err != nil || len(vector) != 2 || math.Abs(float64(vector[0]-0.6)) > 1e-6 || math.Abs(float64(vector[1]-0.8)) > 1e-6 {
		t.Fatalf("unexpected normalized vector: %v, %v", vector, err)
	}
}
