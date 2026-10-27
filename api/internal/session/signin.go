package session

import (
	"context"
	"crypto/hmac"
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
	// signInConfirm: approved with a changed key; the member confirms
	// with a code emailed to the address before a session starts.
	signInConfirm = "confirm"
	// signInEnded is what the browser is told for denied and expired.
	signInEnded = "ended"
)

type signIn struct {
	ID         string    `json:"signin_id"`
	EmailKey   string    `json:"email_key"`
	CodeMAC    string    `json:"code_mac,omitempty"` // the confirm code
	CodeSent   bool      `json:"code_sent,omitempty"`
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
	f := &signIn{ID: si.ID, EmailKey: s.mac("email", addr), MatchCode: si.MatchCode, Stage: signInPending, Exp: s.now().Add(SignInTTL)}
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
// approved (the session is set), confirm (approved with a changed key:
// confirm with an emailed code first) or ended (denied or expired,
// alike). It
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
			// does not reach: a reset or a new phone, or someone else who
			// recovered the account. No session until the member proves
			// the address too (signInConfirm).
			s.log().Warn("sign-in with a changed key", "alarm", "key_changed", "connection_id", f.ConnID)
			f.Stage, f.Exp = signInConfirm, s.now().Add(CodeTTL)
			if err := s.putJSON(ctx, kindSignIn, fk, f, f.Exp, false); err != nil {
				s.fail(w, "store", err)
				return
			}
			setCookie(w, SignInCookie, id, CodeTTL+5*time.Minute)
			writeJSON(w, http.StatusOK, map[string]any{"status": signInConfirm, "expires_at": f.Exp.UTC()})
			return
		}
		if err := s.startSession(ctx, w, r, f.ConnID); errors.Is(err, errRemoved) {
			end(signInEnded)
			return
		} else if err != nil {
			s.fail(w, "session", err)
			return
		}
		end(signInApproved)
	case signInConfirm:
		writeJSON(w, http.StatusOK, map[string]any{"status": signInConfirm, "code_sent": f.CodeSent, "expires_at": f.Exp.UTC()})
	default:
		// Denied and expired look alike to the browser: an unknown address
		// always expires, and a member's vault can turn a challenge down
		// without the member (its ask gate), so "denied" would say the
		// address is a member's (§4.2: the same screen either way).
		end(signInEnded)
	}
}

// confirmFlow reads the sign-in of the request's cookie, locked, if it
// waits for the changed-key confirmation. unlock is nil when it does not
// (the answer is written).
func (s *Server) confirmFlow(w http.ResponseWriter, r *http.Request) (id, fk string, f *signIn, unlock func()) {
	id = cookie(r, SignInCookie)
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_code")
		return "", "", nil, nil
	}
	fk = tokenKey(id)
	mu := s.lock(fk)
	mu.Lock()
	f = &signIn{}
	if ok, err := s.getJSON(r.Context(), kindSignIn, fk, f); err != nil {
		mu.Unlock()
		s.fail(w, "store", err)
		return "", "", nil, nil
	} else if !ok || f.Stage != signInConfirm {
		mu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_code")
		return "", "", nil, nil
	}
	return id, fk, f, mu.Unlock
}

// signInConfirmStart: {email}, after a changed-key approval. Emails a
// code to the address (the one the sign-in was started with, which the
// service vault resolved to this connection). 202 {expires_at} whether or
// not a limit held the code back, as for connect.
func (s *Server) signInConfirmStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id, fk, f, unlock := s.confirmFlow(w, r)
	if unlock == nil {
		return
	}
	defer unlock()
	ctx := r.Context()
	addr := mail.Normalize(req.Email)
	if !hmac.Equal([]byte(s.mac("email", addr)), []byte(f.EmailKey)) {
		writeError(w, http.StatusBadRequest, "wrong_email")
		return
	}
	code, err := s.newCode(ctx, r, f.EmailKey, fk[:16])
	if err != nil {
		s.fail(w, "code", err)
		return
	}
	f.CodeMAC, f.CodeSent = "", true
	if code != "" {
		f.CodeMAC = s.mac("code", id+"\x00"+code)
	}
	f.Exp = s.now().Add(CodeTTL)
	if err := s.putJSON(ctx, kindSignIn, fk, f, f.Exp, false); err != nil {
		s.fail(w, "store", err)
		return
	}
	if err := s.Store.Put(ctx, kindSignInID, f.ID, []byte(fk), f.Exp, false); err != nil {
		s.fail(w, "store", err)
		return
	}
	s.sendCode(addr, code, fk[:16])
	setCookie(w, SignInCookie, id, CodeTTL+5*time.Minute)
	writeJSON(w, http.StatusAccepted, map[string]any{"expires_at": f.Exp.UTC()})
}

// signInConfirmVerify: {email, code}. The right code starts the session.
// Every refusal is invalid_code, with connect's limits (tryCode).
func (s *Server) signInConfirmVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	id, fk, f, unlock := s.confirmFlow(w, r)
	if unlock == nil {
		return
	}
	defer unlock()
	ctx := r.Context()
	okEmail := hmac.Equal([]byte(s.mac("email", mail.Normalize(req.Email))), []byte(f.EmailKey))
	okCode := f.CodeMAC != "" && hmac.Equal([]byte(s.mac("code", id+"\x00"+req.Code)), []byte(f.CodeMAC))
	if ok, err := s.tryCode(ctx, r, "signin-verify:"+fk, f.Exp, okEmail && okCode); err != nil {
		s.fail(w, "rate", err)
		return
	} else if !ok {
		writeError(w, http.StatusBadRequest, "invalid_code")
		return
	}
	end := func() {
		_ = s.Store.Delete(ctx, kindSignIn, fk)
		_ = s.Store.Delete(ctx, kindSignInID, f.ID)
		setCookie(w, SignInCookie, "", 0)
	}
	if err := s.startSession(ctx, w, r, f.ConnID); errors.Is(err, errRemoved) {
		end()
		writeJSON(w, http.StatusOK, map[string]any{"status": signInEnded})
		return
	} else if err != nil {
		s.fail(w, "session", err)
		return
	}
	s.log().Info("changed-key sign-in confirmed", "connection_id", f.ConnID)
	end()
	writeJSON(w, http.StatusOK, map[string]any{"status": signInApproved})
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
