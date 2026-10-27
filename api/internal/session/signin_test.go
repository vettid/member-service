package session

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

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

// The first outcome stands: an approval after a denial is ignored. A
// sign-in for a removed connection makes no session.
func TestSignInDeniedAndRemoved(t *testing.T) {
	f := newFixture(t)
	f.connected()
	f.signInStart()
	f.hook("01EVDENIED", "signin.denied", "c1", map[string]any{"signin_id": "si1", "reason": "denied"})
	f.hook("01EVLATE", "signin.approved", "c1", map[string]any{"signin_id": "si1"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "denied" || f.jar[SessionCookie] != "" {
		t.Fatalf("denied: %s", w.Body)
	}

	f.signInStart()
	f.hook("01EVEXPIRED", "signin.expired", "c1", map[string]any{"signin_id": "si2"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "expired" {
		t.Fatalf("expired: %s", w.Body)
	}

	f.signInStart()
	f.hook("01EVREMOVED", "connection.removed", "c1", map[string]any{"direction": "in"})
	f.hook("01EVAPPROVED", "signin.approved", "c1", map[string]any{"signin_id": "si3"})
	if w := f.do("GET", "/api/signin/status", ""); field(t, w, "status") != "expired" || f.jar[SessionCookie] != "" {
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

// The roster is for members: names and join times, no connection ids.
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
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "c0") || strings.Contains(w.Body.String(), "connection_id") {
		t.Fatalf("members: %d %s", w.Code, w.Body)
	}
	if b := w.Body.String(); !strings.Contains(b, `"count":2`) || strings.Index(b, "Bob") > strings.Index(b, "Ada") {
		t.Fatalf("members: %s", b)
	}
}
