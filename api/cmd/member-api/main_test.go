package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	env := map[string]string{
		"MEMBER_API_ADDR": ":8081", "SERVICE_VAULT_URL": "https://sv:8443", "SERVICE_VAULT_API_KEY": "k", "SERVICE_VAULT_PIN": "sha256/x",
		"WEBHOOK_SECRET": strings.Repeat("w", 32), "MEMBER_API_PEPPER": strings.Repeat("p", 32), "DYNAMODB_TABLE": "t",
		"MAIL_FROM": "no-reply@staging.vettid.org",
	}
	get := func(k string) string { return env[k] }
	if _, err := loadConfig(get); err != nil {
		t.Fatal(err)
	}
	env["MAIL_DIR"] = "/tmp/mail"
	if _, err := loadConfig(get); err == nil {
		t.Fatal("both MAIL_FROM and MAIL_DIR")
	}
	delete(env, "MAIL_DIR")
	env["MEMBER_API_PEPPER"] = "short"
	if _, err := loadConfig(get); err == nil {
		t.Fatal("a short pepper")
	}
	env["MEMBER_API_PEPPER"] = strings.Repeat("p", 32)
	env["CLIENT_IP_HEADER"] = "CloudFront-Viewer-Address"
	if _, err := loadConfig(get); err == nil {
		t.Fatal("a client-address header trusted without the edge's secret")
	}
	env["ORIGIN_VERIFY_SECRET"] = strings.Repeat("o", 32)
	if _, err := loadConfig(get); err != nil {
		t.Fatal(err)
	}
	delete(env, "SERVICE_VAULT_PIN")
	if _, err := loadConfig(get); err == nil || !strings.Contains(err.Error(), "SERVICE_VAULT_PIN") {
		t.Fatalf("missing pin: %v", err)
	}
}

// The visitor's address comes from the edge's header, without the port;
// without a configured header the remote address is used.
func TestClientIP(t *testing.T) {
	f := clientIP("CloudFront-Viewer-Address")
	for v, want := range map[string]string{"198.51.100.7:52311": "198.51.100.7", "2001:db8::7:443": "2001:db8::7", "": ""} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("CloudFront-Viewer-Address", v)
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		if got := f(r); got != want {
			t.Errorf("%q: %q", v, got)
		}
	}
	if clientIP("") != nil {
		t.Fatal("no header: the server's default")
	}
}

// Behind the edge, only requests with its secret header are served
// (health checks excepted).
func TestFromEdge(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := fromEdge([]byte(strings.Repeat("s", 32)), ok)
	for _, c := range []struct {
		path, header string
		want         int
	}{{"/api/me", "", 403}, {"/api/me", "wrong", 403}, {"/api/me", strings.Repeat("s", 32), 204}, {"/api/health", "", 204}} {
		r := httptest.NewRequest("GET", c.path, nil)
		if c.header != "" {
			r.Header.Set(OriginHeader, c.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %q: %d", c.path, c.header, w.Code)
		}
	}
	if fromEdge(nil, ok) == nil {
		t.Fatal("no secret: unwrapped")
	}
}
