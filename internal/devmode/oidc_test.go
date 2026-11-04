package devmode

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestOIDCAuthorizeRejectsNonHTTPRedirect(t *testing.T) {
	issuer, closeServer, err := OIDC("test-client")
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := issuer + "/authorize?client_id=test-client&code_challenge_method=S256&redirect_uri=" + url.QueryEscape("javascript://localhost/callback")
	response, err := client.Get(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || response.Header.Get("Location") != "" {
		t.Fatalf("unsafe redirect accepted: status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
}

func TestOIDCAuthorizeAllowsLoopbackHTTPRedirect(t *testing.T) {
	issuer, closeServer, err := OIDC("test-client")
	if err != nil {
		t.Fatal(err)
	}
	defer closeServer()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	callback := "http://127.0.0.1:8080/auth/callback"
	request := issuer + "/authorize?client_id=test-client&code_challenge_method=S256&state=test&redirect_uri=" + url.QueryEscape(callback)
	response, err := client.Get(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound || !strings.HasPrefix(response.Header.Get("Location"), callback+"?") {
		t.Fatalf("safe redirect rejected: status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
}
