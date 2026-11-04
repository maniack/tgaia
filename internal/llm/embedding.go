package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/maniack/tgaia/internal/config"
	"go.opentelemetry.io/otel"
)

type Embedder struct {
	inputBytes int
	endpoint   string
	model      string
	key        config.Secret
	http       *http.Client
	slots      chan struct{}
}

func NewEmbedder(endpoint, model string, key config.Secret, cfg config.App) (*Embedder, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || model == "" || len(model) > 256 || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid embedding configuration")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1" {
		return nil, errors.New("embedding HTTP is allowed only on loopback")
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	u.Path = path + "/embeddings"
	return &Embedder{inputBytes: cfg.Limit("llm_input_bytes"), endpoint: u.String(), model: model, key: key, http: &http.Client{Timeout: cfg.Duration("llm_timeout"), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, cfg.Limit("llm_concurrency"))}, nil
}

func (e *Embedder) Model() string { return e.model }

func (e *Embedder) Embed(ctx context.Context, input string) ([]float32, error) {
	ctx, span := otel.Tracer("tgaia").Start(ctx, "llm.embedding")
	defer span.End()
	if strings.TrimSpace(input) == "" || len(input) > e.inputBytes {
		return nil, errors.New("embedding input budget exceeded")
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	body, err := json.Marshal(map[string]any{"model": e.model, "input": input, "encoding_format": "float"})
	if err != nil {
		return nil, errors.New("encoding embedding request failed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid embedding request")
	}
	request.Header.Set("Content-Type", "application/json")
	if e.key.Reveal() != "" {
		request.Header.Set("Authorization", "Bearer "+e.key.Reveal())
	}
	response, err := e.http.Do(request)
	if err != nil {
		return nil, errors.New("embedding request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("embedding endpoint returned unsuccessful status")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("invalid embedding response size")
	}
	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Data) != 1 || result.Data[0].Index != 0 || len(result.Data[0].Embedding) < 1 || len(result.Data[0].Embedding) > 4096 {
		return nil, errors.New("invalid embedding response")
	}
	vector := make([]float32, len(result.Data[0].Embedding))
	norm := 0.0
	for i, value := range result.Data[0].Embedding {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("invalid embedding value")
		}
		norm += value * value
		vector[i] = float32(value)
	}
	if norm == 0 {
		return nil, errors.New("empty embedding vector")
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range vector {
		vector[i] *= scale
	}
	return vector, nil
}
