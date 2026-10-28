package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vettid/member-service/api/internal/mail"
	"github.com/vettid/member-service/api/internal/store"
	"github.com/vettid/service-vault/sdk/go/servicevault"
)

func (v *vault) StartSignIn(_ context.Context, req servicevault.SignInRequest) (*servicevault.SignIn, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.starts = append(v.starts, req)
	id := fmt.Sprintf("si%d", len(v.starts))
	v.signins[id] = &servicevault.SignInStatus{ID: id, Status: servicevault.SignInStatusPending}
	return &servicevault.SignIn{ID: id, MatchCode: "42"}, nil
}

func (v *vault) SignInStatus(_ context.Context, id string) (*servicevault.SignInStatus, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.polls++
	st, ok := v.signins[id]
	if !ok {
		return nil, &servicevault.APIError{Status: http.StatusNotFound, Code: servicevault.CodeNotFound}
	}
	c := *st
	return &c, nil
}

func (v *vault) Admins(context.Context) (*servicevault.Admins, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.adminAsk++
	if v.adminErr != nil {
		return nil, v.adminErr
	}
	return &servicevault.Admins{Admins: append([]string(nil), v.admins...), Threshold: 1}, nil
}

// connected makes c1 ("Ada") a member and leaves the browser signed out.
func (f *fixture) connected() {
	f.t.Helper()
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada", "email_bound": true})
	if w := f.do("GET", "/api/connect/status", ""); field(f.t, w, "status") != "connected" {
		f.t.Fatalf("connect: %s", w.Body)
	}
	f.do("POST", "/api/signout", "")
	f.jar = map[string]string{}
}

func (f *fixture) signInStart() {
	f.t.Helper()
	w := f.do("POST", "/api/signin/start", `{"email":" Member@Example.org "}`)
	if w.Code != http.StatusAccepted || field(f.t, w, "match_code") != "42" || f.jar[SignInCookie] == "" {
		f.t.Fatalf("start: %d %s", w.Code, w.Body)
	}
}

// §4.2 end to end: the match code is shown, the approval arrives by
// webhook, the status call sets the session.
func TestSignIn(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	req := f.vault.starts[0]
	if req.Email != addr || len(req.Source) != 64 || strings.Contains(req.Source, f.ip) {
		t.Fatalf("request %+v", req)
	}
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "pending" || field(t, w, "match_code") != "42" {
		t.Fatalf("pending: %s", w.Body)
	}
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me before approval: %d", w.Code)
	}
	if w := f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1", "key_changed": false}); w.Code != http.StatusNoContent {
		t.Fatalf("hook: %d", w.Code)
	}
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "approved" || f.jar[SignInCookie] != "" {
		t.Fatalf("approved: %s", w.Body)
	}
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusOK || field(t, w, "name") != "Ada" {
		t.Fatalf("me: %d %s", w.Code, w.Body)
	}
	if w := f.do("GET", "/api/signin/status", ""); w.Code != http.StatusNotFound {
		t.Fatalf("flow after approval: %d", w.Code)
	}
}

// Without a webhook the status call asks the service vault, at most
// every PollEvery; an approval with a changed key waits for the emailed
// code.
func TestSignInPolls(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	polls := f.vault.polls
	f.do("GET", "/api/signin/status", "")
	f.do("GET", "/api/signin/status", "")
	if f.vault.polls != polls+1 {
		t.Fatalf("polls %d, want %d", f.vault.polls, polls+1)
	}
	changed := true
	f.vault.signins["si1"] = &servicevault.SignInStatus{ID: "si1", Status: servicevault.SignInStatusApproved, ConnectionID: "c1", KeyChanged: &changed}
	f.now = f.now.Add(PollEvery)
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "confirm" || f.jar[SessionCookie] != "" {
		t.Fatalf("changed key: %s", w.Body)
	}
}

// The first outcome stands: an approval after a denial is ignored.
// Denied and expired read alike ("ended"), so the answer does not say
// whether an address is a member's. A sign-in for a removed connection
// makes no session.
func TestSignInDeniedAndRemoved(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	f.hook("01EVDENIED", "signin.denied", "c1", map[string]any{"signin_id": "si1", "reason": "denied"})
	f.hook("01EVLATE", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	// A denial reads pending until SignInWait, as an unknown address
	// does: how soon it ends tells nothing.
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "pending" || field(t, w, "match_code") != "42" {
		t.Fatalf("denied, early: %s", w.Body)
	}
	f.now = f.now.Add(SignInWait)
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("denied: %s", w.Body)
	}

	f.signInStart()
	f.hook("01EVEXPIRED", "signin.expired", "c1", map[string]any{"signin_id": "si2"})
	f.now = f.now.Add(SignInWait)
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" {
		t.Fatalf("expired: %s", w.Body)
	}

	f.signInStart()
	f.hook("01EVREMOVED", "connection.removed", "c1", map[string]any{"direction": "in"})
	f.hook("01EVAPPROVED", "signin.approved", "c1", map[string]any{"signin_id": "si3"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("removed: %s", w.Body)
	}

}

// A sign-in approved after the connection's suspension was handled, and
// before its connection.added (no roster entry yet): no session.
func TestSignInAfterSuspension(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	_ = f.st.Delete(context.Background(), kindMember, "c1") // as if connection.added had not been handled
	f.hook("01EVSUSP", "connection.suspended", "c1", map[string]any{"reason": "rotation_conflict"})
	f.hook("01EVAPPROVED", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("suspended: %s", w.Body)
	}
	if _, ok, _ := f.st.Get(context.Background(), kindMember, "c1"); ok {
		t.Fatal("a roster entry made for a suspended connection")
	}
}

// The suspension marker is kept for good, and an entry marked suspended
// gets no session even without it: a sign-in approved long after ends.
func TestSuspensionLasts(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.hook("01EVSUSP", "connection.suspended", "c1", map[string]any{"reason": "rotation_conflict"})
	f.now = f.now.Add(RemovedTTL + 24*time.Hour)
	if _, ok, _ := f.st.Get(context.Background(), kindSuspended, "c1"); !ok {
		t.Fatal("the suspension marker expired")
	}
	_ = f.st.Delete(context.Background(), kindSuspended, "c1") // the entry's mark alone
	f.signInStart()
	f.hook("01EVAPPROVED", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("a marked entry's sign-in: %s", w.Body)
	}
}

// A browser's sign-in is its own: another browser cannot read or finish
// it, and an unfinished one ends with its time.
func TestSignInFlowIsTheBrowsers(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	mine := f.jar
	f.jar = map[string]string{}
	if w := f.do("GET", "/api/signin/status", ""); w.Code != http.StatusNotFound {
		t.Fatalf("no cookie: %d", w.Code)
	}
	f.jar = mine
	f.now = f.now.Add(SignInTTL)
	if w := f.do("GET", "/api/signin/status", ""); w.Code != http.StatusNotFound {
		t.Fatalf("after its time: %d %s", w.Code, w.Body)
	}
}

func TestSignInLimitsAndChecks(t *testing.T) {
	f := newFixture(t)
	if w := f.do("POST", "/api/signin/start", `{"email":"not an address"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad email: %d", w.Code)
	}
	for i := range SignInsPerSource {
		if w := f.do("POST", "/api/signin/start", `{"email":"m@example.org"}`); w.Code != http.StatusAccepted {
			t.Fatalf("start %d: %d", i, w.Code)
		}
	}
	if w := f.do("POST", "/api/signin/start", `{"email":"m@example.org"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the source limit: %d", w.Code)
	}
	if len(f.vault.starts) != SignInsPerSource {
		t.Fatalf("service vault asked %d times", len(f.vault.starts))
	}
	f.ip = "2001:db8:1:2::9"
	if w := f.do("POST", "/api/signin/start", `{"email":"m@example.org"}`); w.Code != http.StatusAccepted {
		t.Fatalf("another source: %d", w.Code)
	}
}

// The roster is for members: its size to every member, the names and
// join times to admins only, and connection ids to nobody.
func TestMembers(t *testing.T) {
	f := newFixture(t)
	if w := f.do("GET", "/api/members", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", w.Code)
	}
	f.now = f.now.Add(-RateWindow)
	f.hook("01EVBOB", "connection.added", "c0", map[string]any{"invite_id": "x", "name": "Bob"})
	f.now = f.now.Add(RateWindow)
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada"})
	f.do("GET", "/api/connect/status", "")

	w := f.do("GET", "/api/members", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"count":2}` {
		t.Fatalf("member: %d %s", w.Code, w.Body)
	}
	if w := f.do("GET", "/api/me", ""); strings.Contains(w.Body.String(), `"admin":true`) {
		t.Fatalf("me: %s", w.Body)
	}

	// Made an admin: the webhook replaces the cached set at once.
	f.hook("01EVADMIN", "admin_set.changed", "c1", map[string]any{"admins": []string{"c1"}, "added": "c1", "reason": "bootstrap"})
	w = f.do("GET", "/api/members", "")
	b := w.Body.String()
	if !strings.Contains(b, `"count":2`) || !strings.Contains(b, "Ada") || strings.Index(b, "Bob") > strings.Index(b, "Ada") ||
		strings.Contains(b, "c0") || strings.Contains(b, "connection_id") {
		t.Fatalf("admin: %s", b)
	}
	if w := f.do("GET", "/api/me", ""); !strings.Contains(w.Body.String(), `"admin":true`) {
		t.Fatalf("me: %s", w.Body)
	}

	// After the cache's time the service vault is asked again; when it
	// cannot answer, nobody is an admin.
	f.hook("01EVADMIN2", "admin_set.changed", "c0", map[string]any{"admins": []string{"c1"}, "removed": "c0", "reason": "approval"})
	f.now = f.now.Add(AdminsTTL)
	f.vault.adminErr = &servicevault.APIError{Status: http.StatusServiceUnavailable, Code: servicevault.CodeUnavailable}
	if w := f.do("GET", "/api/members", ""); strings.TrimSpace(w.Body.String()) != `{"count":2}` {
		t.Fatalf("vault down: %s", w.Body)
	}
	f.vault.adminErr = nil
	f.vault.admins = []string{"c1"}
	asks := f.vault.adminAsk
	f.do("GET", "/api/members", "")
	if w := f.do("GET", "/api/members", ""); !strings.Contains(w.Body.String(), "Ada") || f.vault.adminAsk != asks+1 {
		t.Fatalf("asked %d times: %s", f.vault.adminAsk-asks, w.Body)
	}
}

// A suspended member (connection.suspended) stays on the roster, marked
// for admins and not counted; their sessions end at the next use. An
// event for a connection not on the roster changes nothing, and the
// member's next connection replaces the entry.
func TestSuspendedMember(t *testing.T) {
	f := newFixture(t)
	f.now = f.now.Add(-RateWindow)
	f.hook("01EVBOB", "connection.added", "c0", map[string]any{"invite_id": "x", "name": "Bob"})
	f.now = f.now.Add(RateWindow)
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada"})
	f.do("GET", "/api/connect/status", "")
	f.hook("01EVADMIN", "admin_set.changed", "c1", map[string]any{"admins": []string{"c1"}, "added": "c1", "reason": "bootstrap"})

	f.hook("01EVSUSP0", "connection.suspended", "c0", map[string]any{"reason": "rotation_conflict"})
	f.hook("01EVSUSPX", "connection.suspended", "cX", map[string]any{"reason": "rotation_conflict"})
	if _, ok, _ := f.st.Get(context.Background(), kindMember, "cX"); ok {
		t.Fatal("a roster entry made for an unknown connection")
	}
	b := f.do("GET", "/api/members", "").Body.String()
	if !strings.Contains(b, `"count":1`) || !strings.Contains(b, `"name":"Bob","joined":`) || !strings.Contains(b, `"suspended":true`) ||
		strings.Count(b, "suspended") != 1 {
		t.Fatalf("admin view: %s", b)
	}

	f.hook("01EVSUSP1", "connection.suspended", "c1", map[string]any{"reason": "rotation_conflict"})
	if w := f.do("GET", "/api/members", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("a suspended member's session: %d", w.Code)
	}
	// Connecting again: the old entry is removed, a new one added.
	f.hook("01EVREM1", "connection.removed", "c1", map[string]any{"direction": "in"})
	f.hook("01EVADD2", "connection.added", "c2", map[string]any{"invite_id": "inv2", "name": "Ada"})
	if _, ok, _ := f.st.Get(context.Background(), kindMember, "c1"); ok {
		t.Fatal("the suspended entry stayed")
	}
	var m Member
	if ok, _ := f.s.getJSON(context.Background(), kindMember, "c2", &m); !ok || m.Suspended {
		t.Fatalf("new entry %+v", m)
	}

	// connection.added handled after connection.suspended (a redelivery,
	// or out of order) keeps the mark.
	f.hook("01EVSUSP3", "connection.suspended", "c3", map[string]any{"reason": "rotation_conflict"})
	f.hook("01EVADD3", "connection.added", "c3", map[string]any{"invite_id": "inv3", "name": "Cy"})
	f.hook("01EVADD0", "connection.added", "c0", map[string]any{"invite_id": "x", "name": "Bob"}) // redelivered after its suspension
	for _, id := range []string{"c3", "c0"} {
		var m Member
		if ok, _ := f.s.getJSON(context.Background(), kindMember, id, &m); !ok || !m.Suspended {
			t.Fatalf("%s: %+v after a late connection.added", id, m)
		}
	}
}

// Events the flow cannot use are acknowledged and change nothing: an
// unknown sign-in, and an approval without a connection id. A sign-in
// the service vault no longer knows ends.
func TestSignInOddEvents(t *testing.T) {
	f := newFixture(t)
	f.connected()
	if w := f.hook("01EVUNKNOWN", "signin.approved", "c1", map[string]any{"signin_id": "nope"}); w.Code != http.StatusNoContent {
		t.Fatalf("unknown sign-in: %d", w.Code)
	}
	f.signInStart()
	f.hook("01EVNOCONN", "signin.approved", "", map[string]any{"signin_id": "si1"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "pending" || f.jar[SessionCookie] != "" {
		t.Fatalf("no connection id: %s", w.Body)
	}
	delete(f.vault.signins, "si1")
	f.now = f.now.Add(SignInWait)
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" {
		t.Fatalf("not found: %s", w.Body)
	}
}

// Signing in ends the browser's earlier session, which may be another
// member's on a shared computer.
func TestSignInEndsTheOldSession(t *testing.T) {
	f := newFixture(t)
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada"})
	f.do("GET", "/api/connect/status", "")
	old := f.jar[SessionCookie]
	f.signInStart()
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	f.do("GET", "/api/signin/status", "")
	if f.jar[SessionCookie] == "" || f.jar[SessionCookie] == old {
		t.Fatal("no new session")
	}
	if _, ok, _ := f.st.Get(context.Background(), kindSession, tokenKey(old)); ok {
		t.Fatal("the old session lives on")
	}
}

// removing is a store that removes c1, as the webhook would, right after
// a session writes its roster entry.
type removing struct {
	*store.Mem
	s *Server
}

func (r *removing) Put(ctx context.Context, kind, id string, data []byte, exp time.Time, ifAbsent bool) error {
	err := r.Mem.Put(ctx, kind, id, data, exp, ifAbsent)
	if kind == kindMember && ifAbsent && err == nil {
		_ = r.s.webhook(ctx, &servicevault.ConnectionRemoved{Header: servicevault.Header{ID: "x", Type: "connection.removed", ConnectionID: id}})
	}
	return err
}

// A removal that lands while a session starts leaves no session and no
// roster entry; a removed member drops off the roster and out of it.
func TestRemovalDuringSignIn(t *testing.T) {
	f := newFixture(t)
	f.signInStart()
	f.s.Store = &removing{Mem: f.st, s: f.s}
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("status: %s", w.Body)
	}
	if _, ok, _ := f.st.Get(context.Background(), kindMember, "c1"); ok {
		t.Fatal("removed connection on the roster")
	}

	f = newFixture(t)
	f.connected()
	f.signInStart()
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	f.do("GET", "/api/signin/status", "")
	if w := f.do("GET", "/api/members", ""); w.Code != http.StatusOK {
		t.Fatalf("members: %d", w.Code)
	}
	f.hook("01EVREMOVED", "connection.removed", "c1", map[string]any{"direction": "in"})
	if w := f.do("GET", "/api/members", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("members after removal: %d", w.Code)
	}
	if items, _ := f.st.List(context.Background(), kindMember); len(items) != 0 {
		t.Fatalf("roster %v", items)
	}
}

// A changed key (a reset, a new phone, or someone who recovered the
// account): no session until the member proves the address with an
// emailed code. Only the browser that started can confirm, only with the
// address it started with, and the code's limits are connect's.
func TestSignInKeyChanged(t *testing.T) {
	f := newFixture(t)
	f.connected()
	sent := f.mail.sent
	f.signInStart()
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1", "key_changed": true})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "confirm" || f.jar[SessionCookie] != "" {
		t.Fatalf("status: %s", w.Body)
	}
	f.hook("01EVLATE", "signin.denied", "c1", map[string]any{"signin_id": "si1", "reason": "denied"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "confirm" {
		t.Fatalf("after a late webhook: %s", w.Body)
	}
	mine := f.jar
	f.jar = map[string]string{}
	if w := f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("another browser: %d", w.Code)
	}
	f.jar = mine
	if w := f.do("POST", "/api/signin/confirm", `{"email":"other@example.org"}`); field(t, w, "error") != "wrong_email" || f.mail.sent != sent {
		t.Fatalf("another address: %s", w.Body)
	}
	if w := f.do("POST", "/api/signin/confirm", `{"email":" Member@Example.org "}`); w.Code != http.StatusAccepted || f.mail.sent != sent+1 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	code := f.mail.code(addr)
	if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"000000"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("wrong code: %s", w.Body)
	}
	if w := f.do("POST", "/api/signin/verify", `{"email":"other@example.org","code":"`+code+`"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("wrong address: %s", w.Body)
	}
	if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"`+code+`"}`); field(t, w, "status") != "approved" || f.jar[SessionCookie] == "" || f.jar[SignInCookie] != "" {
		t.Fatalf("verify: %s", w.Body)
	}
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusOK {
		t.Fatalf("me: %d", w.Code)
	}
}

// The confirm code has connect's limits: CodeAttempts tries, then even
// the right code is refused; a sign-in that never confirms ends.
func TestSignInKeyChangedLimits(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1", "key_changed": true})
	f.do("GET", "/api/signin/status", "")
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	code := f.mail.code(addr)
	for range CodeAttempts {
		f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"999999"}`)
	}
	if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"`+code+`"}`); field(t, w, "error") != "invalid_code" || f.jar[SessionCookie] != "" {
		t.Fatalf("past the attempts: %s", w.Body)
	}
	f.now = f.now.Add(CodeTTL)
	if w := f.do("GET", "/api/signin/status", ""); w.Code != http.StatusNotFound {
		t.Fatalf("after its time: %d", w.Code)
	}
}

// keyChangedSignIn starts a sign-in for c1 and approves it with a changed
// key by webhook, leaving the flow at confirm.
func (f *fixture) keyChangedSignIn(id string) {
	f.t.Helper()
	f.signInStart()
	f.hook(id, "signin.approved", "c1", map[string]any{"signin_id": fmt.Sprintf("si%d", len(f.vault.starts)), "key_changed": true})
	if w := f.do("GET", "/api/signin/status", ""); field(f.t, w, "status") != "confirm" {
		f.t.Fatalf("status: %s", w.Body)
	}
}

// A service vault that does not say whether the key changed: the code is
// asked for.
func TestSignInKeyChangedUnknown(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	f.vault.signins["si1"] = &servicevault.SignInStatus{ID: "si1", Status: servicevault.SignInStatusApproved, ConnectionID: "c1"}
	f.now = f.now.Add(PollEvery)
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "confirm" || f.jar[SessionCookie] != "" {
		t.Fatalf("no key_changed: %s", w.Body)
	}
}

// The confirm routes answer only in the confirm stage; the email says
// what happened; a held-back code still answers 202; a new code replaces
// the last and has its own tries; the confirmation ends ConfirmTTL after
// the approval however often a code is sent; a removed connection gets
// no session.
func TestSignInConfirmRules(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	if w := f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("confirm while pending: %s", w.Body)
	}
	if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"000000"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("verify while pending: %s", w.Body)
	}
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1", "key_changed": true})
	f.do("GET", "/api/signin/status", "")
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	if f.mail.kind != mail.KeyChange {
		t.Fatal("the plain code email")
	}
	first := f.mail.code(addr)
	for range CodeAttempts {
		f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"999999"}`)
	}
	f.now = f.now.Add(time.Minute)
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	second := f.mail.code(addr)
	if first != second {
		if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"`+first+`"}`); field(t, w, "error") != "invalid_code" {
			t.Fatalf("the replaced code: %s", w.Body)
		}
	}
	// Confirm codes per address per hour (their own limit, apart from
	// connect's): these two and one more; the next is held back but
	// answered alike.
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	second = f.mail.code(addr)
	sent := f.mail.sent
	if w := f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`); w.Code != http.StatusAccepted || f.mail.sent != sent {
		t.Fatalf("held back: %d, sent %d", w.Code, f.mail.sent-sent)
	}
	if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"`+second+`"}`); field(t, w, "error") != "invalid_code" {
		t.Fatalf("after a held-back resend: %s", w.Body)
	}

	// ConfirmTTL: resends do not stretch it.
	f = newFixture(t)
	f.connected()
	f.keyChangedSignIn("01EVA")
	for range 4 {
		f.now = f.now.Add(ConfirmTTL / 4)
		f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	}
	if w := f.do("GET", "/api/signin/status", ""); w.Code != http.StatusNotFound {
		t.Fatalf("past ConfirmTTL: %d %s", w.Code, w.Body)
	}

	// Removed before the code is entered.
	f = newFixture(t)
	f.connected()
	f.keyChangedSignIn("01EVB")
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	code := f.mail.code(addr)
	f.hook("01EVREMOVED", "connection.removed", "c1", map[string]any{"direction": "in"})
	if w := f.do("POST", "/api/signin/verify", `{"email":"`+addr+`","code":"`+code+`"}`); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("removed: %s", w.Body)
	}
}

// The alarm line is logged once, when the approval is recorded, whether
// or not the browser asks again; never with the address or the code.
func TestSignInKeyChangedAlarm(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	f.s.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	f.connected()
	f.signInStart()
	f.hook("01EVSIGNIN", "signin.approved", "c1", map[string]any{"signin_id": "si1", "key_changed": true})
	if n := strings.Count(buf.String(), `"alarm":"key_changed"`); n != 1 {
		t.Fatalf("alarm lines after the webhook: %d\n%s", n, buf.String())
	}
	f.do("GET", "/api/signin/status", "")
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	if n := strings.Count(buf.String(), `"alarm":"key_changed"`); n != 1 {
		t.Fatalf("alarm lines: %d", n)
	}
	if strings.Contains(buf.String(), addr) || strings.Contains(buf.String(), f.mail.code(addr)) {
		t.Fatal("the address or the code in the log")
	}
}
