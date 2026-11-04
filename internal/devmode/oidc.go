// Package devmode provides explicit loopback-only development fixtures.
package devmode

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type loginCode struct {
	Nonce, Challenge, Redirect string
	Expires                    time.Time
}

func OIDC(clientID string) (issuer string, cleanup func(), err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", nil, err
	}
	mux := http.NewServeMux()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	issuer = "http://" + listener.Addr().String()
	codes := map[string]loginCode{}
	var mu sync.Mutex
	encode := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}, "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "dev", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || !redirect.IsAbs() || redirect.Scheme != "http" || redirect.User != nil || redirect.Fragment != "" || (redirect.Hostname() != "127.0.0.1" && redirect.Hostname() != "localhost") || q.Get("client_id") != clientID || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "Invalid development login", 400)
			return
		}
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err != nil {
			http.Error(w, "Code generation failed", http.StatusInternalServerError)
			return
		}
		code := base64.RawURLEncoding.EncodeToString(raw[:])
		mu.Lock()
		for k, v := range codes {
			if time.Now().After(v.Expires) {
				delete(codes, k)
			}
		}
		codes[code] = loginCode{Nonce: q.Get("nonce"), Challenge: q.Get("code_challenge"), Redirect: redirect.String(), Expires: time.Now().Add(time.Minute)}
		mu.Unlock()
		query := redirect.Query()
		query.Set("code", code)
		query.Set("state", q.Get("state"))
		redirect.RawQuery = query.Encode()
		// #nosec G710 -- redirect is an absolute HTTP URL restricted above to loopback hosts.
		http.Redirect(w, r, redirect.String(), 302)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid token request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		entry, ok := codes[r.Form.Get("code")]
		delete(codes, r.Form.Get("code"))
		mu.Unlock()
		digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		id, _, _ := r.BasicAuth()
		if id == "" {
			id = r.Form.Get("client_id")
		}
		if !ok || time.Now().After(entry.Expires) || id != clientID || r.Form.Get("redirect_uri") != entry.Redirect || base64.RawURLEncoding.EncodeToString(digest[:]) != entry.Challenge {
			http.Error(w, "Invalid code", 400)
			return
		}
		body := encode(map[string]any{"alg": "RS256", "kid": "dev", "typ": "JWT"}) + "." + encode(map[string]any{"iss": issuer, "aud": clientID, "sub": "local-admin", "groups": []string{"local-admins"}, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": entry.Nonce})
		hash := sha256.Sum256([]byte(body))
		signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
		if err != nil {
			http.Error(w, "Signing failed", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":"development-only","token_type":"Bearer","expires_in":3600,"id_token":%q}`, body+"."+base64.RawURLEncoding.EncodeToString(signature))
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	return issuer, func() { _ = server.Close(); <-done }, nil
}
