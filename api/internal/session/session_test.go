package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vettid/member-service/api/internal/store"
	"github.com/vettid/service-vault/sdk/go/servicevault"
	"github.com/vettid/service-vault/sdk/go/webhook"
)

var (
	t0     = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	secret = bytes.Repeat([]byte{0x42}, 32) // TEST ONLY
)

// mailbox is the fake mailer: the last code per address.
type mailbox struct {
	mu    sync.Mutex
	codes map[string]string
	sent  int
}

func (m *mailbox) SendCode(_ context.Context, to, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[to] = code
	m.sent++
	return nil
}

func (m *mailbox) code(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.codes[to]
}

// vault is the fake service vault.
type vault struct {
	mu       sync.Mutex
	invites  []string // emails, verified
	status   map[string]*servicevault.InviteStatus
	polls    int
	inviteNo int
	now      func() time.Time
	signins  map[string]*servicevault.SignInStatus
	starts   []servicevault.SignInRequest
	admins   []string
	adminErr error
	adminAsk int
}

func (v *vault) CreateInvite(_ context.Context, email string, ev servicevault.EmailVerification) (*servicevault.Invite, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if ev != servicevault.EmailVerified {
		return nil, fmt.Errorf("unverified")
	}
	v.invites = append(v.invites, email)
	v.inviteNo++
	id := fmt.Sprintf("inv%d", v.inviteNo)
	v.status[id] = &servicevault.InviteStatus{ID: id, Status: servicevault.InviteStatusWaiting}
	return &servicevault.Invite{ID: id, URL: "https://vettid.org/i/" + id, ExpiresAt: v.now().Add(10 * time.Minute)}, nil
}

func (v *vault) InviteStatus(_ context.Context, id string) (*servicevault.InviteStatus, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.polls++
	st := *v.status[id]
	return &st, nil
}

type fixture struct {
	t     *testing.T
	s     *Server
	h     http.Handler
	mail  *mailbox
	vault *vault
	st    *store.Mem
	now   time.Time
	jar   map[string]string // the browser's cookies
	ip    string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, mail: &mailbox{codes: map[string]string{}}, vault: &vault{status: map[string]*servicevault.InviteStatus{}, signins: map[string]*servicevault.SignInStatus{}},
		st: store.NewMem(), now: t0, jar: map[string]string{}, ip: "198.51.100.7"}
	f.st.Now = func() time.Time { return f.now }
	f.vault.now = f.st.Now
	f.s = &Server{Store: f.st, Vault: f.vault, Mail: f.mail, Pepper: bytes.Repeat([]byte{7}, 32),
		Webhook:  &servicevault.Receiver{Secret: secret, Now: func() time.Time { return f.now }},
		ClientIP: func(*http.Request) string { return f.ip },
		Now:      func() time.Time { return f.now }, Go: func(fn func()) { fn() }}
	f.h = f.s.Handler()
	return f
}

// do sends a request with the browser's cookies and keeps the ones set.
func (f *fixture) do(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range f.jar {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(f.jar, c.Name)
		} else {
			f.jar[c.Name] = c.Value
		}
	}
	return w
}

func field(t *testing.T, w *httptest.ResponseRecorder, k string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("%s: %v", w.Body, err)
	}
	s, _ := m[k].(string)
	return s
}

// hook delivers a signed webhook event.
func (f *fixture) hook(id, typ, conn string, data any) *httptest.ResponseRecorder {
	f.t.Helper()
	d, _ := json.Marshal(data)
	body, _ := json.Marshal(map[string]any{"id": id, "type": typ, "connection_id": conn, "created_at": f.now, "data": json.RawMessage(d)})
	sig, err := webhook.Sign(secret, f.now, id, body)
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/webhooks/service-vault", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(webhook.HeaderEventID, id)
	r.Header.Set(webhook.HeaderSignature, sig)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

const addr = "member@example.org"

// verify starts a flow for addr and enters the mailed code.
func (f *fixture) verify() string {
	f.t.Helper()
	if w := f.do("POST", "/api/connect/start", `{"email":" Member@Example.org "}`); w.Code != http.StatusAccepted {
		f.t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	code := f.mail.code(addr)
	if len(code) != 6 {
		f.t.Fatalf("code %q", code)
	}
	w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+code+`"}`)
	if w.Code != http.StatusOK {
		f.t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
	return field(f.t, w, "url")
}

// §4.1 end to end: the code proves the address, the invitation is
// verified, the SAS and the connection arrive by webhook, the status
// call sets the session, sign-out ends it.
func TestConnect(t *testing.T) {
	f := newFixture(t)
	if w := f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`); w.Code != http.StatusAccepted || f.jar[FlowCookie] == "" {
		t.Fatalf("start: %d", w.Code)
	}
	code := f.mail.code(addr)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	for _, body := range []string{
		`{"email":"` + addr + `","code":"` + wrong + `"}`,
		`{"email":"other@example.org","code":"` + code + `"}`,
	} {
		if w := f.do("POST", "/api/connect/verify", body); w.Code != http.StatusBadRequest || field(t, w, "error") != "invalid_code" {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if len(f.vault.invites) != 0 {
		t.Fatal("an invitation before the code")
	}
	w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+code+`"}`)
	if w.Code != http.StatusOK || field(t, w, "url") != "https://vettid.org/i/inv1" || f.vault.invites[0] != addr {
		t.Fatalf("verify: %d %s %v", w.Code, w.Body, f.vault.invites)
	}
	// The code is spent.
	if w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+code+`"}`); w.Code != http.StatusBadRequest {
		t.Fatal("a code worked twice")
	}
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "waiting" {
		t.Fatalf("status: %s", w.Body)
	}
	if w := f.hook("01EVPENDING", "connection.pending", "c1", map[string]string{"invite_id": "inv1", "sas": "123456", "name": "Ada Lovelace"}); w.Code != http.StatusNoContent {
		t.Fatalf("webhook: %d %s", w.Code, w.Body)
	}
	f.now = f.now.Add(time.Second) // within PollEvery of the last poll
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "sas" || field(t, w, "sas") != "123456" {
		t.Fatalf("status: %s", w.Body)
	}
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada Lovelace", "email_bound": true})
	w = f.do("GET", "/api/connect/status", "")
	if field(t, w, "status") != "connected" || f.jar[SessionCookie] == "" || f.jar[FlowCookie] != "" {
		t.Fatalf("connected: %s %v", w.Body, f.jar)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie && (!c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "") {
			t.Fatalf("session cookie %+v", c)
		}
	}
	w = f.do("GET", "/api/me", "")
	if w.Code != http.StatusOK || field(t, w, "name") != "Ada Lovelace" || field(t, w, "connection_id") != "c1" {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	if w := f.do("POST", "/api/signout", ""); w.Code != http.StatusNoContent || f.jar[SessionCookie] != "" {
		t.Fatalf("signout: %d", w.Code)
	}
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me after signout: %d", w.Code)
	}
}

// A late webhook: the status call asks the service vault itself, at most
// every PollEvery, and still connects.
func TestConnectPolls(t *testing.T) {
	f := newFixture(t)
	f.verify()
	f.vault.status["inv1"] = &servicevault.InviteStatus{ID: "inv1", Status: servicevault.InviteStatusConnected, ConnectionID: "c9"}
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "connected" || f.vault.polls != 1 {
		t.Fatalf("status %s after %d polls", w.Body, f.vault.polls)
	}
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusOK || field(t, w, "connection_id") != "c9" {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	// The connection.added webhook then fills in the name.
	f.hook("01EVADDED9", "connection.added", "c9", map[string]any{"invite_id": "inv1", "name": "Grace Hopper", "email_bound": true})
	if w := f.do("GET", "/api/me", ""); field(t, w, "name") != "Grace Hopper" {
		t.Fatalf("me: %s", w.Body)
	}
}

// Limits: an address gets CodesPerEmail codes an hour, a flow
// CodeAttempts guesses; past a limit the answers are the same as below
// it, but no code is sent and none works.
func TestConnectLimits(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < CodesPerEmail; i++ {
		f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	}
	sent := f.mail.sent
	w := f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	if w.Code != http.StatusAccepted || f.mail.sent != sent {
		t.Fatalf("over the limit: %d, sent %d -> %d", w.Code, sent, f.mail.sent)
	}
	if w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+f.mail.code(addr)+`"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("a limited flow accepted a code: %s", w.Body)
	}
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "code" {
		t.Fatalf("limited status: %s", w.Body)
	}
	// After the window, a new code; five wrong guesses spend the flow.
	f.now = f.now.Add(RateWindow)
	f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	code := f.mail.code(addr)
	for i := 0; i < CodeAttempts; i++ {
		f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"x`+fmt.Sprint(i)+`"}`)
	}
	if w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+code+`"}`); field(t, w, "error") != "invalid_code" {
		t.Fatal("the right code after five wrong ones")
	}
	// Per source.
	f.now = f.now.Add(RateWindow)
	sent = f.mail.sent
	for i := 0; i < CodesPerSource+1; i++ {
		f.do("POST", "/api/connect/start", fmt.Sprintf(`{"email":"m%d@example.org"}`, i))
	}
	if f.mail.sent-sent != CodesPerSource {
		t.Fatalf("%d codes from one source", f.mail.sent-sent)
	}
	if w := f.do("POST", "/api/connect/start", `{"email":"not an address"}`); field(t, w, "error") != "bad_email" {
		t.Fatalf("bad address: %s", w.Body)
	}
}

// Removal ends the member's sessions; a declined invitation ends the
// flow without one.
func TestRemovedAndDeclined(t *testing.T) {
	f := newFixture(t)
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada", "email_bound": true})
	f.do("GET", "/api/connect/status", "")
	f.hook("01EVREMOVED", "connection.removed", "c1", map[string]any{"direction": "in"})
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me after removal: %d", w.Code)
	}
	f.jar = map[string]string{}
	f.now = f.now.Add(RateWindow)
	f.verify()
	f.hook("01EVDECLINED", "connection.declined", "", map[string]any{"invite_id": "inv2"})
	f.now = f.now.Add(PollEvery)
	f.vault.status["inv2"].Status = servicevault.InviteStatusDeclined
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "declined" || f.jar[SessionCookie] != "" {
		t.Fatalf("declined: %s", w.Body)
	}
}

// Webhooks: a bad signature is refused, a redelivery is not handled
// twice (the replay cache is in the store, so it survives a restart).
func TestWebhookChecks(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest("POST", "/api/webhooks/service-vault", strings.NewReader(`{"id":"x","type":"connection.removed"}`))
	r.Header.Set(webhook.HeaderEventID, "x")
	r.Header.Set(webhook.HeaderSignature, "t=1,v1=00")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", w.Code)
	}
	if w := f.hook("01EVONE", "connection.removed", "c1", map[string]any{"direction": "in"}); w.Code != http.StatusNoContent {
		t.Fatalf("first: %d", w.Code)
	}
	if _, ok, _ := f.st.Get(context.Background(), kindHook, "01EVONE"); !ok {
		t.Fatal("event id not kept in the store")
	}
	if w := f.hook("01EVONE", "connection.removed", "c1", map[string]any{"direction": "in"}); w.Code != http.StatusOK {
		t.Fatalf("replay: %d", w.Code)
	}
}

// A POST must be JSON (with SameSite=Strict cookies, this refuses
// cross-site forms); unknown members are refused.
func TestRequestChecks(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest("POST", "/api/connect/start", strings.NewReader(`email=a@example.org`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form post: %d", w.Code)
	}
	if w := f.do("POST", "/api/connect/start", `{"email":"a@example.org","x":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown member: %d", w.Code)
	}
	if w := f.do("GET", "/api/connect/status", ""); w.Code != http.StatusNotFound {
		t.Fatalf("status without a flow: %d", w.Code)
	}
}

// A connection removed while its flow still says connected never gets a
// session, nor a roster entry back.
func TestRemovedBeforeSession(t *testing.T) {
	f := newFixture(t)
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada", "email_bound": true})
	f.hook("01EVREMOVED", "connection.removed", "c1", map[string]any{"direction": "in"})
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "expired" || f.jar[SessionCookie] != "" {
		t.Fatalf("status after removal: %s %v", w.Body, f.jar)
	}
	if _, ok, _ := f.st.Get(context.Background(), kindMember, "c1"); ok {
		t.Fatal("the removed connection is back on the roster")
	}
	// A late connection.added for it changes nothing either.
	f.hook("01EVADDED2", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada", "email_bound": true})
	if _, ok, _ := f.st.Get(context.Background(), kindMember, "c1"); ok {
		t.Fatal("a late connection.added brought it back")
	}
	// A late decline does not undo a connection.
	f.jar = map[string]string{}
	f.now = f.now.Add(RateWindow)
	f.verify()
	f.hook("01EVADDED3", "connection.added", "c2", map[string]any{"invite_id": "inv2", "name": "Bo", "email_bound": true})
	f.hook("01EVDECLINED3", "connection.declined", "", map[string]any{"invite_id": "inv2"})
	if w := f.do("GET", "/api/connect/status", ""); field(t, w, "status") != "connected" {
		t.Fatalf("a decline after the connection: %s", w.Body)
	}
}

// Wrong codes: past FailedPerSource in the hour, that source's verifies
// are refused, even with the right code, while another source's work;
// across all sources, reaching FailedAlarm only logs.
func TestFailedVerifies(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Truncate(RateWindow)
	guess := func(i int) {
		f.jar = map[string]string{}
		f.do("POST", "/api/connect/start", fmt.Sprintf(`{"email":"g%d@example.org"}`, i))
		f.do("POST", "/api/connect/verify", fmt.Sprintf(`{"email":"g%d@example.org","code":"x"}`, i))
	}
	// From one /64 (its flows are past the codes-per-source limit after
	// 20, but wrong codes still count).
	for i := 0; i < FailedPerSource; i++ {
		f.ip = fmt.Sprintf("2001:db8:9:9::%x", i+1)
		guess(i)
	}
	f.jar = map[string]string{}
	f.ip = "203.0.113.1"
	f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	code := f.mail.code(addr)
	f.ip = "2001:db8:9:9::ffff"
	if w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+code+`"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("a source past its wrong codes verified: %s", w.Body)
	}
	f.ip = "198.51.100.200"
	if w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+code+`"}`); w.Code != http.StatusOK {
		t.Fatalf("another source was held up: %s", w.Body)
	}
	// Across sources, far past FailedAlarm: nobody else is refused.
	for i := 0; i < FailedAlarm+10; i++ {
		f.ip = fmt.Sprintf("198.18.%d.%d", i/250, i%250)
		guess(1000 + i)
	}
	f.jar = map[string]string{}
	f.ip = "203.0.113.2"
	f.do("POST", "/api/connect/start", `{"email":"late@example.org"}`)
	if w := f.do("POST", "/api/connect/verify", `{"email":"late@example.org","code":"`+f.mail.code("late@example.org")+`"}`); w.Code != http.StatusOK {
		t.Fatalf("a right code refused after many wrong ones elsewhere: %s", w.Body)
	}
}

// Sources: an IPv6 /64 is one source; a source over its limit does not
// use up an address's codes; an address gets CodesPerDay codes a day.
func TestSourcesAndDailyCap(t *testing.T) {
	f := newFixture(t)
	sent := f.mail.sent
	for i := 0; i < CodesPerSource+5; i++ {
		f.ip = fmt.Sprintf("2001:db8:1:2::%x", i+1)
		f.do("POST", "/api/connect/start", fmt.Sprintf(`{"email":"v%d@example.org"}`, i))
	}
	if f.mail.sent-sent != CodesPerSource {
		t.Fatalf("%d codes from one /64", f.mail.sent-sent)
	}
	// Still over: the victim's address is not charged.
	f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	f.ip = "203.0.113.5"
	sent = f.mail.sent
	f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	if f.mail.sent != sent+1 {
		t.Fatal("a source over its limit used up the address's codes")
	}
	// The daily cap: CodesPerEmail an hour, CodesPerDay in all.
	f.now = f.now.Add(RateWindow)
	n := 1
	for h := 0; h < 23; h++ {
		f.ip = fmt.Sprintf("203.0.113.%d", 10+h)
		for i := 0; i < CodesPerEmail; i++ {
			before := f.mail.sent
			f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
			n += f.mail.sent - before
		}
		f.now = f.now.Add(RateWindow)
	}
	if n != CodesPerDay {
		t.Fatalf("%d codes in a day", n)
	}
}

func TestSourceKey(t *testing.T) {
	for ip, want := range map[string]string{
		"198.51.100.7": "198.51.100.7", "::ffff:198.51.100.7": "198.51.100.7",
		"2001:db8:1:2::7": "2001:db8:1:2::/64", "2001:db8:1:2:ffff::1": "2001:db8:1:2::/64", "junk": "junk",
	} {
		if got := sourceKey(ip); got != want {
			t.Errorf("%s: %s", ip, got)
		}
	}
}
