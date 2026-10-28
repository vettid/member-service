package session

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vettid/service-vault/sdk/go/servicevault"
)

// An event whose replay record was left pending (our write retried after
// its answer was lost, or a failed handler not removed) is handled, not
// taken for a replay; a handled one is a replay.
func TestWebhookPendingRecordHandled(t *testing.T) {
	f := newFixture(t)
	_ = f.st.Put(context.Background(), kindHook, "01EVREM", hookPending, f.now.Add(time.Hour), false)
	if w := f.hook("01EVREM", "connection.removed", "c9", map[string]any{"direction": "in"}); w.Code != http.StatusNoContent {
		t.Fatalf("pending record: %d", w.Code)
	}
	if _, removed, _ := f.st.Get(context.Background(), kindRemoved, "c9"); !removed {
		t.Fatal("the event was not handled")
	}
	if w := f.hook("01EVREM", "connection.removed", "c9", map[string]any{"direction": "in"}); w.Code != http.StatusOK {
		t.Fatalf("a handled event again: %d", w.Code)
	}
}

// A session ends when its connection was removed, even if a race put
// the roster entry back.
func TestSessionEndsOnRemovedMarker(t *testing.T) {
	f := newFixture(t)
	f.verify()
	f.hook("01EVADDED", "connection.added", "c1", map[string]any{"invite_id": "inv1", "name": "Ada"})
	f.do("GET", "/api/connect/status", "")
	_ = f.st.Put(context.Background(), kindRemoved, "c1", nil, f.now.Add(RemovedTTL), false)
	if w := f.do("GET", "/api/me", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("me with the removed marker: %d", w.Code)
	}
}

// A source past StartsPerSource connect starts an hour is refused (429)
// before anything is written for it.
func TestConnectStartsPerSource(t *testing.T) {
	f := newFixture(t)
	for i := range StartsPerSource {
		if w := f.do("POST", "/api/connect/start", fmt.Sprintf(`{"email":"m%d@example.org"}`, i)); w.Code != http.StatusAccepted {
			t.Fatalf("start %d: %d", i, w.Code)
		}
	}
	flows := func() int { items, _ := f.st.List(context.Background(), kindFlow); return len(items) }
	before := flows()
	if w := f.do("POST", "/api/connect/start", `{"email":"x@example.org"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", w.Code)
	}
	if flows() != before {
		t.Fatal("a flow was written for a refused start")
	}
}

// Connect starts with a member's address cannot use up the codes of the
// member's changed-key confirmation.
func TestConfirmCodesApartFromConnect(t *testing.T) {
	f := newFixture(t)
	f.connected()
	for i := range CodesPerDay {
		f.ip = fmt.Sprintf("2001:db8:%x::1", i+10) // many sources
		f.jar = map[string]string{}
		f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`)
	}
	f.ip = "198.51.100.7"
	f.jar = map[string]string{}
	f.keyChangedSignIn("01EVKC")
	sent := f.mail.sent
	f.do("POST", "/api/signin/confirm", `{"email":"`+addr+`"}`)
	if f.mail.sent != sent+1 {
		t.Fatal("the confirmation code was held back by connect starts")
	}
}

// An admin_set.changed older than the one applied (a late redelivery)
// does not bring back an older set.
func TestAdminSetOutOfOrder(t *testing.T) {
	f := newFixture(t)
	f.hook("01EVNEW", "admin_set.changed", "c2", map[string]any{"admins": []string{"c2"}, "reason": "approval"})
	f.now = f.now.Add(-time.Minute)
	f.hook("01EVOLD", "admin_set.changed", "c1", map[string]any{"admins": []string{"c1"}, "reason": "bootstrap"})
	f.now = f.now.Add(time.Minute)
	if f.s.isAdmin(context.Background(), "c1") || !f.s.isAdmin(context.Background(), "c2") {
		t.Fatal("the older admin set came back")
	}
}

// Only an https invitation link reaches the page (the button and the QR
// code carry it).
func TestInviteURLMustBeHTTPS(t *testing.T) {
	f := newFixture(t)
	f.vault.inviteURL = "intent://scan/#Intent;scheme=x;end"
	if w := f.do("POST", "/api/connect/start", `{"email":"`+addr+`"}`); w.Code != http.StatusAccepted {
		t.Fatal(w.Code)
	}
	w := f.do("POST", "/api/connect/verify", `{"email":"`+addr+`","code":"`+f.mail.code(addr)+`"}`)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "intent:") {
		t.Fatalf("a non-https link: %d %s", w.Code, w.Body)
	}
	_ = servicevault.EmailVerified
}
