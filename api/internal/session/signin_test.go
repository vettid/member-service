package session

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

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
// every PollEvery; a changed key still signs in.
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
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "approved" || f.jar[SessionCookie] == "" {
		t.Fatalf("approved: %s", w.Body)
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
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "ended" || f.jar[SessionCookie] != "" {
		t.Fatalf("denied: %s", w.Body)
	}

	f.signInStart()
	f.hook("01EVEXPIRED", "signin.expired", "c1", map[string]any{"signin_id": "si2"})
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
	f.now = f.now.Add(PollEvery)
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
