package webauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/devmode"
)

func TestOIDCLoginAndCSRF(t *testing.T) {
	issuer, stop, err := devmode.OIDC("test-client")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var auth *Auth
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	auth, err = New(context.Background(), Config{Issuer: issuer, ClientID: "test-client", Callback: server.URL + "/callback", SessionKey: strings.Repeat("s", 40), AdminGroups: []string{"local-admins"}})
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/login", auth.Login)
	mux.HandleFunc("/callback", auth.Callback)
	mux.Handle("/", auth.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		group, csrf := Session(r)
		json.NewEncoder(w).Encode(map[string]string{"group": group, "csrf": csrf})
	})))
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	response, err := client.Get(server.URL + "/api/private")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("unauthenticated API available")
	}
	response, err = client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	var session map[string]string
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		t.Fatal("login did not reach authenticated handler")
	}
	response.Body.Close()
	if session["group"] != "local-admins" || session["csrf"] == "" {
		t.Fatal("OIDC identity not verified")
	}
	for _, valid := range []bool{false, true} {
		req, _ := http.NewRequest("POST", server.URL+"/api/write", strings.NewReader(`{}`))
		req.Header.Set("Origin", server.URL)
		if valid {
			req.Header.Set("X-CSRF-Token", session["csrf"])
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := 403
		if valid {
			want = 200
		}
		if response.StatusCode != want {
			t.Fatalf("CSRF status %d want %d", response.StatusCode, want)
		}
	}
	auth.groups = map[string]bool{"someone-else": true}
	response, err = client.Get(server.URL + "/api/private")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("removed administrator retains authorization")
	}
}

func TestSessionRequiresSubjectAndAuthorizedGroup(t *testing.T) {
	a := &Auth{key: []byte(strings.Repeat("x", 40)), groups: map[string]bool{"admins": true}}
	nextCalled := false
	handler := a.Protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))

	for name, session := range map[string]ticket{
		"missing subject": {Group: "admins", Expires: time.Now().Add(time.Minute).Unix()},
		"removed group":   {Subject: "user", Group: "former-admins", Expires: time.Now().Add(time.Minute).Unix()},
	} {
		t.Run(name, func(t *testing.T) {
			nextCalled = false
			r := httptest.NewRequest("GET", "/api/private", nil)
			r.AddCookie(&http.Cookie{Name: "tgaia_session", Value: a.sign(session)})
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || nextCalled {
				t.Fatalf("session accepted: status=%d next=%v", w.Code, nextCalled)
			}
		})
	}
}
func TestTamperedTicket(t *testing.T) {
	a := &Auth{key: []byte(strings.Repeat("x", 40))}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "s", Value: "tampered.signature"})
	if _, err := a.read(r, "s"); err == nil {
		t.Fatal("tampered session accepted")
	}
}

func TestOIDCRejectsUserOutsideAdminGroups(t *testing.T) {
	issuer, stop, err := devmode.OIDC("test-client")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	auth, err := New(context.Background(), Config{Issuer: issuer, ClientID: "test-client", Callback: server.URL + "/callback", SessionKey: strings.Repeat("s", 40), AdminGroups: []string{"other-admins"}})
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/login", auth.Login)
	mux.HandleFunc("/callback", auth.Callback)
	client := &http.Client{Jar: mustCookieJar(t)}
	response, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin group accepted: status=%d", response.StatusCode)
	}
}

func mustCookieJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return jar
}
