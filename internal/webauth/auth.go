package webauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type Config struct {
	Issuer, ClientID, ClientSecret, Callback, SessionKey string
	AdminGroups                                          []string
}
type Auth struct {
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	key      []byte
	groups   map[string]bool
	secure   bool
	origin   string
	issuer   string
}
type ticket struct {
	Subject  string `json:"sub,omitempty"`
	Group    string `json:"group,omitempty"`
	State    string `json:"state,omitempty"`
	Nonce    string `json:"nonce,omitempty"`
	Verifier string `json:"verifier,omitempty"`
	CSRF     string `json:"csrf,omitempty"`
	Expires  int64  `json:"exp"`
}

func New(ctx context.Context, c Config) (*Auth, error) {
	callback, err := url.Parse(c.Callback)
	if err != nil || callback.Host == "" || len(c.SessionKey) < 32 || len(c.AdminGroups) == 0 {
		return nil, errors.New("invalid OIDC/session configuration")
	}
	if callback.Scheme != "https" && !(callback.Scheme == "http" && (callback.Hostname() == "127.0.0.1" || callback.Hostname() == "localhost")) {
		return nil, errors.New("OIDC callback requires HTTPS or local loopback")
	}
	issuer, err := url.Parse(c.Issuer)
	if err != nil || issuer.Host == "" || (issuer.Scheme != "https" && !(issuer.Scheme == "http" && (issuer.Hostname() == "127.0.0.1" || issuer.Hostname() == "localhost"))) {
		return nil, errors.New("OIDC issuer requires HTTPS or local loopback")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	ctx = oidc.ClientContext(ctx, client)
	provider, err := oidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery failed")
	}
	groups := map[string]bool{}
	for _, group := range c.AdminGroups {
		if group != "" {
			groups[group] = true
		}
	}
	if len(groups) == 0 {
		return nil, errors.New("invalid OIDC/session configuration")
	}
	return &Auth{oauth: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, Endpoint: provider.Endpoint(), RedirectURL: c.Callback, Scopes: []string{oidc.ScopeOpenID, "groups"}}, verifier: provider.Verifier(&oidc.Config{ClientID: c.ClientID}), key: []byte(c.SessionKey), groups: groups, secure: callback.Scheme == "https", origin: callback.Scheme + "://" + callback.Host, issuer: c.Issuer}, nil
}
func random() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("entropy unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func (a *Auth) sign(t ticket) string {
	data, _ := json.Marshal(t)
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *Auth) read(r *http.Request, name string) (ticket, error) {
	var t ticket
	c, err := r.Cookie(name)
	if err != nil {
		return t, errors.New("missing cookie")
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 {
		return t, errors.New("invalid cookie")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return t, err
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return t, errors.New("invalid signature")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(data, &t) != nil || t.Expires <= time.Now().Unix() {
		return t, errors.New("expired cookie")
	}
	return t, nil
}
func (a *Auth) cookie(w http.ResponseWriter, name string, t ticket) {
	// #nosec G124 -- Secure is enabled for every HTTPS callback; insecure cookies
	// are permitted only by the separately validated loopback mock-OIDC profile.
	http.SetCookie(w, &http.Cookie{Name: name, Value: a.sign(t), Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(t.Expires - time.Now().Unix())})
}
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	t := ticket{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Expires: time.Now().Add(5 * time.Minute).Unix()}
	a.cookie(w, "tgaia_login", t)
	http.Redirect(w, r, a.oauth.AuthCodeURL(t.State, oidc.Nonce(t.Nonce), oauth2.S256ChallengeOption(t.Verifier)), http.StatusFound)
}
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	t, err := a.read(r, "tgaia_login")
	// #nosec G124 -- see cookie(): production callbacks require HTTPS, while the
	// development exception is restricted to loopback.
	http.SetCookie(w, &http.Cookie{Name: "tgaia_login", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode})
	if err != nil || !hmac.Equal([]byte(r.URL.Query().Get("state")), []byte(t.State)) || t.State == "" {
		http.Error(w, "Login state invalid", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	token, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(t.Verifier))
	if err != nil {
		http.Error(w, "OIDC exchange failed", 401)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "Missing ID token", 401)
		return
	}
	id, err := a.verifier.Verify(ctx, raw)
	if err != nil || id.Nonce != t.Nonce || id.Subject == "" {
		http.Error(w, "Administrator access denied", 403)
		return
	}
	var claims struct {
		Groups []string `json:"groups"`
	}
	if id.Claims(&claims) != nil {
		http.Error(w, "Administrator access denied", 403)
		return
	}
	group := ""
	for _, candidate := range claims.Groups {
		if a.groups[candidate] {
			group = candidate
			break
		}
	}
	if group == "" {
		http.Error(w, "Administrator access denied", 403)
		return
	}
	a.cookie(w, "tgaia_session", ticket{Subject: id.Subject, Group: group, CSRF: random(), Expires: min(time.Now().Add(time.Hour).Unix(), id.Expiry.Unix())})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (a *Auth) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, err := a.read(r, "tgaia_session")
		if err != nil || t.Subject == "" || !a.groups[t.Group] {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "Authentication required", 401)
			} else {
				http.Redirect(w, r, "/login", 302)
			}
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Origin") != a.origin || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(t.CSRF)) {
				http.Error(w, "CSRF check failed", 403)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionKey{}, t)))
	})
}

type sessionKey struct{}

func Session(r *http.Request) (string, string) {
	t, _ := r.Context().Value(sessionKey{}).(ticket)
	return t.Group, t.CSRF
}
