package webui

import (
	"strings"
	"testing"
)

func TestAuditRedactsCredentials(t *testing.T) {
	for _, input := range []string{`{"name":"llm.api_key","value":"SECRET"}`, `{"definition":{"token":"SECRET","env":{"KEY":"SECRET"},"args":["SECRET"],"url":"SECRET"}}`, `{"name":"oidc.client_secret","value":"SECRET"}`, `{"result":"SECRET"}`} {
		if strings.Contains(auditRequest([]byte(input)), "SECRET") {
			t.Fatal("credential leaked")
		}
	}
	if !strings.Contains(auditRequest([]byte(`{"role":"reader","user_id":1}`)), "reader") {
		t.Fatal("change redacted")
	}
}
