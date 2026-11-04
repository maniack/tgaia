package webui

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/google/uuid"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
	"github.com/maniack/tgaia/internal/webauth"
)

// Record exact requested changes, with credential-bearing fields removed.
func auditRequest(raw []byte) string {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return `{"invalid_json":true}`
	}
	var redact func(map[string]any)
	redact = func(m map[string]any) {
		for k, v := range m {
			key := strings.ToLower(k)
			if key == "token" || key == "api_key" || key == "webhook_secret" || key == "env" || key == "args" || key == "url" || key == "result" {
				m[k] = "[redacted]"
				continue
			}
			if nested, ok := v.(map[string]any); ok {
				redact(nested)
			}
		}
		if name, ok := m["name"].(string); ok && (config.SecretSetting(name) || strings.HasPrefix(name, "oidc.") || strings.HasPrefix(name, "database.") || strings.Contains(name, "secret") || strings.Contains(name, "endpoint")) {
			if _, exists := m["value"]; exists {
				m["value"] = "[redacted]"
			}
		}
	}
	redact(value)
	b, _ := json.Marshal(value)
	return string(b)
}
func (s *Server) mutation(fn func(*Server, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64000))
		if err != nil {
			http.Error(w, "request too large", 413)
			return
		}
		var target map[string]any
		_ = json.Unmarshal(body, &target)
		if r.URL.Path == "/api/tasks" && r.Method == "POST" && target != nil && (target["id"] == nil || target["id"] == "") {
			target["id"] = uuid.NewString()
			body, _ = json.Marshal(target)
		}
		group, _ := webauth.Session(r)
		record := auditRequest(body)
		response := httptest.NewRecorder()
		failed := errors.New("mutation rejected")
		err = s.DB.Transaction(r.Context(), func(db *storage.Database) error {
			before, err := db.AuditState(r.Context(), r.URL.Path, target)
			if err != nil {
				return err
			}
			copy := *s
			copy.DB = db
			if s.Settings != nil {
				copy.Settings = s.Settings.InDatabase(db)
			}
			req := r.Clone(r.Context())
			req.Body = io.NopCloser(bytes.NewReader(body))
			fn(&copy, response, req)
			if response.Code >= 400 {
				return failed
			}
			after, err := db.AuditState(r.Context(), r.URL.Path, target)
			if err != nil {
				return err
			}
			return db.Audit(r.Context(), group, r.Method+" "+r.URL.Path, record, response.Code, before, after)
		})
		if errors.Is(err, failed) {
			err = s.DB.Audit(r.Context(), group, r.Method+" "+r.URL.Path, record, response.Code)
		}
		if err != nil {
			http.Error(w, "mutation or audit persistence failed", 503)
			return
		}
		for k, v := range response.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}
}
