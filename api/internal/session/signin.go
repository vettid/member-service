package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/vettid/member-service/api/internal/mail"
	"github.com/vettid/service-vault/sdk/go/servicevault"
)

// Sign-in (MEMBER-SERVICE-PLAN §4.2). The service vault looks the address
// up, answers the same way whether or not it is known (and whether or not
// one of its per-email or per-source limits was reached), and sends the
// challenge with the match code to the member's vault. The member API
// only shows the match code and waits for the outcome.
const (
	// SignInTTL is how long the browser waits: the challenge's five-minute
	// exp (plan 0.6.1) and a minute for a late answer.
	SignInTTL = 6 * time.Minute
	// SignInsPerSource caps sign-ins from one client address (an IPv6 /64)
	// per hour, before the service vault is asked. It is per source only,
	// so it says nothing about an address.
	SignInsPerSource = 30
)

// SignInCookie holds the browser's sign-in flow (HttpOnly, like the
// others).
const SignInCookie = "__Host-ms_signin"

// Record kinds of sign-in.
const (
	kindSignIn   = "signin"    // flow id -> signIn
	kindSignInID = "signin-id" // service vault sign-in id -> flow id
)

// Sign-in stages; the status answers with them.
const (
	signInPending  = "pending"
	signInApproved = "approved"
	signInDenied   = "denied"
	signInExpired  = "expired"
	// signInEnded is what the browser is told for denied and expired.
	signInEnded = "ended"
)

type signIn struct {
	ID         string    `json:"signin_id"`
	MatchCode  string    `json:"match_code"`
	Stage      string    `json:"stage"`
	ConnID     string    `json:"connection_id,omitempty"`
	KeyChanged bool      `json:"key_changed,omitempty"`
	Polled     time.Time `json:"polled,omitempty"`
	Exp        time.Time `json:"exp"`
}

// signInStart: {email}. 202 {match_code, expires_at} and a sign-in
// cookie, the same for a known and an unknown address.
func (s *Server) signInStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	addr := mail.Normalize(req.Email)
	if !mail.Valid(addr) {
		writeError(w, http.StatusBadRequest, "bad_email")
		return
	}
	ctx := r.Context()
	// The service vault's per-source limit gets a keyed hash of the
	// source, not the address itself (64 hex characters).
	src := s.mac("source", sourceKey(s.clientIP(r)))
	if over, err := s.limited(ctx, "signin:source:"+src, SignInsPerSource, RateWindow); err != nil {
		s.fail(w, "rate", err)
		return
	} else if over {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	si, err := s.Vault.StartSignIn(ctx, servicevault.SignInRequest{Email: addr, Source: src})
	if err != nil {
		s.log().Warn("start sign-in", "err", errName(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	id, err := token()
	if err != nil {
		s.fail(w, "token", err)
		return
	}
	fk := tokenKey(id)
	f := &signIn{ID: si.ID, MatchCode: si.MatchCode, Stage: signInPending, Exp: s.now().Add(SignInTTL)}
	if err := s.putJSON(ctx, kindSignIn, fk, f, f.Exp, true); err != nil {
		s.fail(w, "store", err)
		return
	}
	if err := s.Store.Put(ctx, kindSignInID, si.ID, []byte(fk), f.Exp, false); err != nil {
		s.fail(w, "store", err)
		return
	}
	setCookie(w, SignInCookie, id, SignInTTL)
	writeJSON(w, http.StatusAccepted, map[string]any{"match_code": f.MatchCode, "expires_at": f.Exp.UTC()})
}

// signInStatus reports the sign-in: pending (show the match code),
// approved (the session is set) or ended (denied or expired, alike). It
// asks the service vault itself at most every PollEvery, in case a
// webhook is late.
func (s *Server) signInStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := cookie(r, SignInCookie)
	if id == "" {
		writeError(w, http.StatusNotFound, "no_flow")
		return
	}
	fk := tokenKey(id)
	mu := s.lock(fk)
	f := &signIn{}
	read := func() bool {
		if ok, err := s.getJSON(ctx, kindSignIn, fk, f); err != nil {
			s.fail(w, "store", err)
			return false
		} else if !ok {
			setCookie(w, SignInCookie, "", 0)
			writeError(w, http.StatusNotFound, "no_flow")
			return false
		}
		return true
	}
	mu.Lock()
	defer mu.Unlock()
	if !read() {
		return
	}
	if now := s.now(); f.Stage == signInPending && now.Sub(f.Polled) >= PollEvery {
		f.Polled = now
		if err := s.putJSON(ctx, kindSignIn, fk, f, f.Exp, false); err != nil {
			s.fail(w, "store", err)
			return
		}
		// Without the lock, so a slow service vault holds up no webhook;
		// the answer applies only if the sign-in is still pending.
		mu.Unlock()
		st, err := s.Vault.SignInStatus(ctx, f.ID)
		mu.Lock()
		if !read() {
			return
		}
		switch {
		case servicevault.IsNotFound(err):
			st, err = &servicevault.SignInStatus{ID: f.ID, Status: servicevault.SignInStatusExpired}, nil
		case err != nil:
			s.log().Warn("sign-in status", "err", errName(err))
		}
		if err == nil && f.Stage == signInPending {
			applySignInStatus(f, st)
			if err := s.putJSON(ctx, kindSignIn, fk, f, f.Exp, false); err != nil {
				s.fail(w, "store", err)
				return
			}
		}
	}
	end := func(status string) {
		_ = s.Store.Delete(ctx, kindSignIn, fk)
		_ = s.Store.Delete(ctx, kindSignInID, f.ID)
		setCookie(w, SignInCookie, "", 0)
		writeJSON(w, http.StatusOK, map[string]any{"status": status})
	}
	switch f.Stage {
	case signInPending:
		writeJSON(w, http.StatusOK, map[string]any{"status": signInPending, "match_code": f.MatchCode, "expires_at": f.Exp.UTC()})
	case signInApproved:
		if f.KeyChanged {
			// Approved with a credential key the connection's pinned one
			// does not reach (a reset or a new phone). Still a sign-in;
			// logged for review.
			s.log().Warn("sign-in with a changed key", "connection_id", f.ConnID)
		}
		if err := s.startSession(ctx, w, r, f.ConnID); errors.Is(err, errRemoved) {
			end(signInEnded)
			return
		} else if err != nil {
			s.fail(w, "session", err)
			return
		}
		end(signInApproved)
	default:
		// Denied and expired look alike to the browser: an unknown address
		// always expires, and a member's vault can turn a challenge down
		// without the member (its ask gate), so "denied" would say the
		// address is a member's (§4.2: the same screen either way).
		end(signInEnded)
	}
}

func applySignInStatus(f *signIn, st *servicevault.SignInStatus) {
	switch st.Status {
	case servicevault.SignInStatusApproved:
		if st.ConnectionID != "" {
			f.Stage, f.ConnID = signInApproved, st.ConnectionID
			f.KeyChanged = st.KeyChanged != nil && *st.KeyChanged
		}
	case servicevault.SignInStatusDenied:
		f.Stage = signInDenied
	case servicevault.SignInStatusExpired:
		f.Stage = signInExpired
	}
}

// updateSignIn applies change to the flow of a service vault sign-in,
// if any, while it is pending: the first outcome stands.
func (s *Server) updateSignIn(ctx context.Context, signInID string, change func(*signIn)) error {
	fk, ok, err := s.Store.Get(ctx, kindSignInID, signInID)
	if err != nil || !ok {
		return err
	}
	mu := s.lock(string(fk))
	mu.Lock()
	defer mu.Unlock()
	f := &signIn{}
	found, err := s.getJSON(ctx, kindSignIn, string(fk), f)
	if err != nil || !found || f.Stage != signInPending {
		return err
	}
	change(f)
	return s.putJSON(ctx, kindSignIn, string(fk), f, f.Exp, false)
}

// --- roster (§6) ---

// AdminsTTL is how long the admin set is kept between asks; an
// admin_set.changed webhook replaces it at once.
const AdminsTTL = time.Minute

// adminCache is the service vault's admin set as last seen.
type adminCache struct {
	mu    sync.Mutex
	set   map[string]bool
	until time.Time
	gen   uint64 // counts webhook updates
}

// setAdmins keeps an admin set. An answer to an ask (gen is the count
// when it started) does not replace a webhook's newer set.
func (s *Server) setAdmins(ids []string, fromHook bool, gen uint64) {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	s.admins.mu.Lock()
	defer s.admins.mu.Unlock()
	if fromHook {
		s.admins.gen++
	} else if gen != s.admins.gen {
		return
	}
	s.admins.set, s.admins.until = set, s.now().Add(AdminsTTL)
}

// isAdmin reports whether a connection is in the admin set. When the
// service vault cannot say, the answer is no.
func (s *Server) isAdmin(ctx context.Context, connID string) bool {
	s.admins.mu.Lock()
	if s.admins.set != nil && s.now().Before(s.admins.until) {
		ok := s.admins.set[connID]
		s.admins.mu.Unlock()
		return ok
	}
	gen := s.admins.gen
	s.admins.mu.Unlock()
	a, err := s.Vault.Admins(ctx)
	if err != nil {
		s.log().Warn("admins", "err", errName(err))
		return false
	}
	s.setAdmins(a.Admins, false, gen)
	return slices.Contains(a.Admins, connID)
}

// members answers a signed-in member with the roster's size; an admin
// also gets the names and join times, oldest first. No connection ids.
func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conn, _, ok, err := s.Member(ctx, r)
	if err != nil {
		s.fail(w, "store", err)
		return
	} else if !ok {
		writeError(w, http.StatusUnauthorized, "signed_out")
		return
	}
	items, err := s.Store.List(ctx, kindMember)
	if err != nil {
		s.fail(w, "store", err)
		return
	}
	if !s.isAdmin(ctx, conn) {
		writeJSON(w, http.StatusOK, map[string]any{"count": len(items)})
		return
	}
	type entry struct {
		Name   string    `json:"name"`
		Joined time.Time `json:"joined"`
	}
	out := make([]entry, 0, len(items))
	for _, it := range items {
		var m Member
		if json.Unmarshal(it.Data, &m) != nil {
			continue
		}
		out = append(out, entry{Name: m.Name, Joined: m.Joined.UTC()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Joined.Before(out[j].Joined) })
	writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "members": out})
}
