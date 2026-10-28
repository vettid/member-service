package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vettid/service-vault/sdk/go/webhook"
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
	env["ORIGIN_VERIFY_SECRET_PREVIOUS"] = "short"
	if _, err := loadConfig(get); err == nil {
		t.Fatal("a short previous origin secret")
	}
	env["ORIGIN_VERIFY_SECRET_PREVIOUS"] = strings.Repeat("q", 32)
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
	s, prev := strings.Repeat("s", 32), strings.Repeat("p", 32)
	type tc struct {
		path, header string
		want         int
	}
	for previous, cases := range map[string][]tc{
		"": {{"/api/me", "", 403}, {"/api/me", "wrong", 403}, {"/api/me", s, 204}, {"/api/me", prev, 403}, {"/api/health", "", 204}},
		// During a rotation both the new and the previous secret pass.
		prev: {{"/api/me", "", 403}, {"/api/me", "wrong", 403}, {"/api/me", s, 204}, {"/api/me", prev, 204}},
	} {
		h := fromEdge([]byte(s), []byte(previous), slog.New(slog.DiscardHandler), ok)
		for _, c := range cases {
			r := httptest.NewRequest("GET", c.path, nil)
			if c.header != "" {
				r.Header.Set(OriginHeader, c.header)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("previous %q, %s %q: %d", previous, c.path, c.header, w.Code)
			}
		}
	}
	// A request with the previous value is logged (once a minute), so a
	// rotation can see when the edge has stopped sending it.
	var buf bytes.Buffer
	h := fromEdge([]byte(s), []byte(prev), slog.New(slog.NewTextHandler(&buf, nil)), ok)
	for range 3 {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Header.Set(OriginHeader, prev)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.Header.Set(OriginHeader, s)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if n := strings.Count(buf.String(), "previous origin secret"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, buf.String())
	}
	if fromEdge(nil, nil, nil, ok) == nil {
		t.Fatal("no secret: unwrapped")
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := healthcheck(strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatal(err)
	}
	// The task's own address has no host (":8081"): the probe asks
	// 127.0.0.1.
	if err := healthcheck(":" + srv.URL[strings.LastIndexByte(srv.URL, ':')+1:]); err != nil {
		t.Fatalf("empty host: %v", err)
	}
	if err := healthcheck("127.0.0.1:1"); err == nil {
		t.Fatal("nothing listening")
	}
	if err := healthcheck("nonsense"); err == nil {
		t.Fatal("bad address")
	}
}

// The CDK forwards these header names to /api/* (cdk WEBHOOK_HEADERS):
// they must be the SDK's.
func TestWebhookHeaderNames(t *testing.T) {
	src, err := os.ReadFile("../../../cdk/lib/member-service-stack.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{webhook.HeaderSignature, webhook.HeaderEventID} {
		if !strings.Contains(string(src), "'"+h+"'") {
			t.Fatalf("the CDK does not forward %s", h)
		}
	}
}
