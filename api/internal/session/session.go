// Package session is how a visitor becomes a member of member.vettid.org
// and gets a session (MEMBER-SERVICE-PLAN §4.1, §4.2): connect with
// VettID after proving control of an email address, and later sign in
// by email with a match code approved on the phone. It talks to the
// service vault only through its SDK and verified webhooks.
//
// The member API never stores an email address: a flow keeps a keyed
// hash of it, and the browser sends the address again with the code.
// Logs carry ids, types and error codes only.
package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/vettid/member-service/api/internal/mail"
	"github.com/vettid/member-service/api/internal/store"
	"github.com/vettid/service-vault/sdk/go/servicevault"
)

// Limits and lifetimes (chosen; MEMBER-SERVICE-PLAN §4.1 leaves them to
// the application).
const (
	CodeTTL        = 10 * time.Minute // a code works this long
	CodeAttempts   = 5                // wrong codes per flow
	CodesPerEmail  = 3                // codes per address per hour
	CodesPerDay    = 10               // codes per address per day
	CodesPerSource = 20               // codes per client address (an IPv6 /64) per hour
	// FailedPerSource caps wrong codes from one client address (an IPv6
	// /64) per hour: past it that source's verifies are refused until the
	// hour ends. Only the guesser is held up.
	FailedPerSource = 30
	// FailedAlarm: when wrong codes across all flows reach it in an hour,
	// an ERROR is logged for an alarm. Nothing is refused because of it, so
	// it cannot be used to keep everyone out.
	FailedAlarm = 200
	RateWindow  = time.Hour
	dayWindow   = 24 * time.Hour
	SessionTTL  = 12 * time.Hour
	// PollEvery bounds how often a status request asks the service vault
	// itself, in case a webhook is late.
	PollEvery = 2 * time.Second
	maxBody   = 4 << 10
)

// Cookie names: __Host- (Secure, Path=/, no Domain) and HttpOnly; the
// site's JavaScript never sees either.
const (
	SessionCookie = "__Host-ms_session"
	FlowCookie    = "__Host-ms_flow"
)

// Record kinds.
const (
	kindFlow    = "flow"    // flow id -> flow
	kindInvite  = "invite"  // invite id -> flow id
	kindSession = "session" // SHA-256 of the session token -> session
	kindMember  = "member"  // connection id -> member (the roster)
	kindRate    = "rate"    // limit key -> counter
	kindHook    = "hook"    // webhook event id (replay)
	kindRemoved = "removed" // connection id -> "": removed, never a session again
)

// RemovedTTL is how long a removed connection is remembered: longer than
// any flow lives.
const RemovedTTL = 30 * 24 * time.Hour

// Vault is the part of the service vault's SDK client the server uses.
type Vault interface {
	CreateInvite(ctx context.Context, email string, v servicevault.EmailVerification) (*servicevault.Invite, error)
	InviteStatus(ctx context.Context, id string) (*servicevault.InviteStatus, error)
	StartSignIn(ctx context.Context, req servicevault.SignInRequest) (*servicevault.SignIn, error)
	SignInStatus(ctx context.Context, id string) (*servicevault.SignInStatus, error)
}

// Server serves the member API's session routes.
type Server struct {
	Store store.Store
	Vault Vault
	Mail  mail.Mailer
	// Pepper keys the hashes of addresses and codes (at least 32 bytes).
	Pepper []byte
	// Webhook verifies the service vault's webhooks; its Replay is set
	// by Handler when nil.
	Webhook *servicevault.Receiver
	// ClientIP returns the visitor's address for the per-source limit
	// (default: the connection's remote host).
	ClientIP func(*http.Request) string
	Now      func() time.Time
	Log      *slog.Logger
	// Go runs background work (sending a code); default a goroutine.
	Go func(func())

	// locks serialize read-modify-write of one flow between requests and
	// webhooks, striped by the flow's key so that one slow call holds up
	// only the flows that share its stripe. One task runs the member API
	// (MEMBER-SERVICE-PLAN §9); counters that must hold across tasks
	// (attempts, limits) are the store's atomic Incr.
	locks [256]sync.Mutex
}

// lock returns the stripe of a flow key (hex).
func (s *Server) lock(fk string) *sync.Mutex {
	var b byte
	if len(fk) >= 2 {
		if v, err := hex.DecodeString(fk[:2]); err == nil {
			b = v[0]
		}
	}
	return &s.locks[b]
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Server) goAsync(f func()) {
	if s.Go != nil {
		s.Go(f)
		return
	}
	go f()
}

func (s *Server) clientIP(r *http.Request) string {
	if s.ClientIP != nil {
		if ip := s.ClientIP(r); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// sourceKey is the rate-limit key of a client address: an IPv6 address
// counts as its /64 (one subscriber's prefix), anything else as itself.
func sourceKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

// mac is the keyed hash of a labelled value.
func (s *Server) mac(label, v string) string {
	h := hmac.New(sha256.New, s.Pepper)
	h.Write([]byte(label))
	h.Write([]byte{0})
	h.Write([]byte(v))
	return hex.EncodeToString(h.Sum(nil))
}

// Handler returns the routes under /api.
func (s *Server) Handler() http.Handler {
	if s.Webhook != nil && s.Webhook.Replay == nil {
		s.Webhook.Replay = &replay{s: s.Store}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/connect/start", s.connectStart)
	mux.HandleFunc("POST /api/connect/verify", s.connectVerify)
	mux.HandleFunc("GET /api/connect/status", s.connectStatus)
	mux.HandleFunc("POST /api/signin/start", s.signInStart)
	mux.HandleFunc("GET /api/signin/status", s.signInStatus)
	mux.HandleFunc("GET /api/me", s.me)
	mux.HandleFunc("GET /api/members", s.members)
	mux.HandleFunc("POST /api/signout", s.signOut)
	if s.Webhook != nil {
		mux.Handle("POST /api/webhooks/service-vault", s.Webhook.Handler(s.webhook))
	}
	return mux
}

// --- responses and requests ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// readJSON decodes a small JSON body into v. A POST must say it is JSON:
// a cross-site form cannot, so with SameSite=Strict cookies this closes
// cross-site requests.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "json_required")
		return false
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request")
		return false
	}
	return true
}

func token() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func tokenKey(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if maxAge > 0 {
		c.MaxAge = int(maxAge / time.Second)
	} else {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func cookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// --- records ---

// Flow stages.
const (
	stageCode      = "code"      // a code was sent
	stageLimited   = "limited"   // no code was sent (a limit); answers as stageCode
	stageInvite    = "invite"    // verified; the invitation is out
	stageSAS       = "sas"       // the handshake ran; compare the SAS
	stageConnected = "connected" // the connection is active
	stageDeclined  = "declined"
	stageExpired   = "expired"
)

type flow struct {
	EmailKey  string    `json:"email_key"`
	CodeMAC   string    `json:"code_mac,omitempty"`
	Stage     string    `json:"stage"`
	InviteID  string    `json:"invite_id,omitempty"`
	InviteURL string    `json:"invite_url,omitempty"`
	InviteExp time.Time `json:"invite_exp,omitempty"`
	SAS       string    `json:"sas,omitempty"`
	ConnID    string    `json:"connection_id,omitempty"`
	Polled    time.Time `json:"polled,omitempty"`
	Exp       time.Time `json:"exp"`
}

type sessionRecord struct {
	ConnID  string    `json:"connection_id"`
	Created time.Time `json:"created"`
}

// Member is a roster entry: a connection (MEMBER-SERVICE-PLAN §6).
type Member struct {
	Name       string    `json:"name"`
	Joined     time.Time `json:"joined"`
	EmailBound bool      `json:"email_bound"`
}

func (s *Server) getJSON(ctx context.Context, kind, id string, v any) (bool, error) {
	b, ok, err := s.Store.Get(ctx, kind, id)
	if err != nil || !ok {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func (s *Server) putJSON(ctx context.Context, kind, id string, v any, exp time.Time, ifAbsent bool) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Store.Put(ctx, kind, id, b, exp, ifAbsent)
}

// limited counts one use against key in a fixed window and reports
// whether it is over n.
func (s *Server) limited(ctx context.Context, key string, n int, window time.Duration) (bool, error) {
	c, err := s.Store.Incr(ctx, kindRate, key, s.now().Add(window))
	return c > n, err
}

// --- connect (§4.1) ---

// connectStart: {email}. Always 202 and a flow cookie, whether or not a
// code was sent (a limit), so the answer says nothing about the address.
func (s *Server) connectStart(w http.ResponseWriter, r *http.Request) {
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
	now := s.now()
	f := &flow{EmailKey: s.mac("email", addr), Stage: stageCode, Exp: now.Add(CodeTTL)}
	// The source first: a source over its limit does not use up the
	// address's codes (so it cannot lock someone out of theirs).
	overSource, err := s.limited(ctx, "code:source:"+s.mac("source", sourceKey(s.clientIP(r))), CodesPerSource, RateWindow)
	if err != nil {
		s.fail(w, "rate", err)
		return
	}
	overEmail := false
	if !overSource {
		for _, l := range []struct {
			key string
			n   int
			win time.Duration
		}{{"code:email:", CodesPerEmail, RateWindow}, {"code:email-day:", CodesPerDay, dayWindow}} {
			over, err := s.limited(ctx, l.key+f.EmailKey, l.n, l.win)
			if err != nil {
				s.fail(w, "rate", err)
				return
			}
			overEmail = overEmail || over
		}
	}
	id, err := token()
	if err != nil {
		s.fail(w, "token", err)
		return
	}
	var code string
	if overEmail || overSource {
		f.Stage = stageLimited
		s.log().Info("connect code limited", "flow", tokenKey(id)[:16], "email", overEmail, "source", overSource)
	} else {
		n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
		if err != nil {
			s.fail(w, "code", err)
			return
		}
		code = leftPad6(n.Int64())
		f.CodeMAC = s.mac("code", id+"\x00"+code)
	}
	if err := s.putJSON(ctx, kindFlow, tokenKey(id), f, f.Exp, true); err != nil {
		s.fail(w, "store", err)
		return
	}
	if code != "" {
		fk := tokenKey(id)[:16]
		s.goAsync(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := s.Mail.SendCode(ctx, addr, code); err != nil {
				s.log().Warn("connect code not sent", "flow", fk, "err", errName(err))
			}
		})
	}
	setCookie(w, FlowCookie, id, CodeTTL+5*time.Minute)
	writeJSON(w, http.StatusAccepted, map[string]any{"expires_at": f.Exp.UTC()})
}

func leftPad6(n int64) string {
	b := []byte("000000")
	for i := 5; i >= 0 && n > 0; i-- {
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b)
}

// connectVerify: {email, code}. A right code for the flow's address
// makes the invitation, with email_verified: the address is proven.
// Every refusal is the same invalid_code.
func (s *Server) connectVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email string `json:"email"`
		Code  string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	id := cookie(r, FlowCookie)
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_code")
		return
	}
	fk := tokenKey(id)
	mu := s.lock(fk)
	mu.Lock()
	defer mu.Unlock()
	f := &flow{}
	if ok, err := s.getJSON(ctx, kindFlow, fk, f); err != nil {
		s.fail(w, "store", err)
		return
	} else if !ok || f.Stage != stageCode && f.Stage != stageLimited {
		writeError(w, http.StatusBadRequest, "invalid_code")
		return
	}
	// A source past its wrong codes for the hour is refused, right or
	// wrong, until the hour ends.
	hour := s.now().UTC().Truncate(RateWindow).Format(time.RFC3339)
	srcFailed := "verify:failed-source:" + s.mac("source", sourceKey(s.clientIP(r))) + ":" + hour
	if failed, err := s.Store.Count(ctx, kindRate, srcFailed); err != nil {
		s.fail(w, "rate", err)
		return
	} else if failed >= FailedPerSource {
		writeError(w, http.StatusBadRequest, "invalid_code")
		return
	}
	// Every try counts, atomically, before the code is compared: the
	// count holds even if two tasks overlap.
	n, err := s.Store.Incr(ctx, kindRate, "verify:"+fk, f.Exp)
	if err != nil {
		s.fail(w, "rate", err)
		return
	}
	addr := mail.Normalize(req.Email)
	okEmail := hmac.Equal([]byte(s.mac("email", addr)), []byte(f.EmailKey))
	okCode := f.Stage == stageCode && hmac.Equal([]byte(s.mac("code", id+"\x00"+req.Code)), []byte(f.CodeMAC))
	if n > CodeAttempts || !okEmail || !okCode {
		if _, err := s.Store.Incr(ctx, kindRate, srcFailed, s.now().Add(RateWindow)); err != nil {
			s.fail(w, "rate", err)
			return
		}
		g, err := s.Store.Incr(ctx, kindRate, "verify:failed:"+hour, s.now().Add(RateWindow))
		if err != nil {
			s.fail(w, "rate", err)
			return
		}
		if g == FailedAlarm {
			s.log().Error("wrong codes reached the alarm level this hour", "alarm", "verify_failed", "count", FailedAlarm)
		}
		writeError(w, http.StatusBadRequest, "invalid_code")
		return
	}
	inv, err := s.Vault.CreateInvite(ctx, addr, servicevault.EmailVerified)
	if err != nil {
		s.log().Warn("create invite", "err", errName(err))
		writeError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	f.Stage, f.CodeMAC, f.InviteID, f.InviteURL, f.InviteExp = stageInvite, "", inv.ID, inv.URL, inv.ExpiresAt
	f.Exp = inv.ExpiresAt.Add(5 * time.Minute) // the status page outlives the invitation a little
	if err := s.putJSON(ctx, kindFlow, fk, f, f.Exp, false); err != nil {
		s.fail(w, "store", err)
		return
	}
	if err := s.Store.Put(ctx, kindInvite, inv.ID, []byte(fk), f.Exp, false); err != nil {
		s.fail(w, "store", err)
		return
	}
	setCookie(w, FlowCookie, id, f.Exp.Sub(s.now()))
	writeJSON(w, http.StatusOK, map[string]any{"url": inv.URL, "expires_at": inv.ExpiresAt.UTC()})
}

// connectStatus reports the flow: code (waiting for the code), waiting
// (the invitation is out), sas (compare it), connected (the session is
// set), declined or expired. It asks the service vault itself at most
// every PollEvery, in case a webhook is late.
func (s *Server) connectStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := cookie(r, FlowCookie)
	if id == "" {
		writeError(w, http.StatusNotFound, "no_flow")
		return
	}
	mu := s.lock(tokenKey(id))
	mu.Lock()
	defer mu.Unlock()
	f := &flow{}
	if ok, err := s.getJSON(ctx, kindFlow, tokenKey(id), f); err != nil {
		s.fail(w, "store", err)
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "no_flow")
		return
	}
	now := s.now()
	if (f.Stage == stageInvite || f.Stage == stageSAS) && now.Sub(f.Polled) >= PollEvery {
		f.Polled = now
		if st, err := s.Vault.InviteStatus(ctx, f.InviteID); err != nil {
			s.log().Warn("invite status", "err", errName(err))
		} else {
			applyInviteStatus(f, st)
		}
		if err := s.putJSON(ctx, kindFlow, tokenKey(id), f, f.Exp, false); err != nil {
			s.fail(w, "store", err)
			return
		}
	}
	switch f.Stage {
	case stageCode, stageLimited:
		writeJSON(w, http.StatusOK, map[string]any{"status": "code", "expires_at": f.Exp.UTC()})
	case stageInvite:
		writeJSON(w, http.StatusOK, map[string]any{"status": "waiting", "url": f.InviteURL, "expires_at": f.InviteExp.UTC()})
	case stageSAS:
		writeJSON(w, http.StatusOK, map[string]any{"status": "sas", "sas": f.SAS, "expires_at": f.InviteExp.UTC()})
	case stageConnected:
		if err := s.startSession(ctx, w, f.ConnID); errors.Is(err, errRemoved) {
			// Removed since: no session, ever.
			_ = s.Store.Delete(ctx, kindFlow, tokenKey(id))
			setCookie(w, FlowCookie, "", 0)
			writeJSON(w, http.StatusOK, map[string]any{"status": stageExpired})
			return
		} else if err != nil {
			s.fail(w, "session", err)
			return
		}
		_ = s.Store.Delete(ctx, kindFlow, tokenKey(id))
		_ = s.Store.Delete(ctx, kindInvite, f.InviteID)
		setCookie(w, FlowCookie, "", 0)
		writeJSON(w, http.StatusOK, map[string]any{"status": "connected"})
	default:
		setCookie(w, FlowCookie, "", 0)
		writeJSON(w, http.StatusOK, map[string]any{"status": f.Stage})
	}
}

func applyInviteStatus(f *flow, st *servicevault.InviteStatus) {
	switch st.Status {
	case servicevault.InviteStatusSAS:
		if f.Stage == stageInvite {
			f.Stage, f.SAS = stageSAS, st.SAS
		}
	case servicevault.InviteStatusConnected:
		f.Stage, f.ConnID = stageConnected, st.ConnectionID
	case servicevault.InviteStatusDeclined:
		f.Stage = stageDeclined
	case servicevault.InviteStatusExpired:
		f.Stage = stageExpired
	}
}

// --- sessions ---

// errRemoved: the connection was removed; it never gets a session again.
var errRemoved = errors.New("session: connection removed")

// startSession makes a session for the connection and sets its cookie.
// The roster entry is created if the connection.added webhook has not
// arrived yet (it fills in the name), unless the connection was removed
// in the meantime (errRemoved).
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, connID string) error {
	if connID == "" {
		return errors.New("session: no connection")
	}
	if _, removed, err := s.Store.Get(ctx, kindRemoved, connID); err != nil {
		return err
	} else if removed {
		return errRemoved
	}
	now := s.now()
	if err := s.putJSON(ctx, kindMember, connID, &Member{Joined: now.UTC()}, time.Time{}, true); err != nil && !errors.Is(err, store.ErrExists) {
		return err
	}
	t, err := token()
	if err != nil {
		return err
	}
	if err := s.putJSON(ctx, kindSession, tokenKey(t), &sessionRecord{ConnID: connID, Created: now.UTC()}, now.Add(SessionTTL), true); err != nil {
		return err
	}
	setCookie(w, SessionCookie, t, SessionTTL)
	s.log().Info("session started", "connection_id", connID)
	return nil
}

// Member returns the member of the request's session, or ok false. A
// session whose connection left the roster (removed) ends.
func (s *Server) Member(ctx context.Context, r *http.Request) (connID string, m *Member, ok bool, err error) {
	t := cookie(r, SessionCookie)
	if t == "" {
		return "", nil, false, nil
	}
	var sr sessionRecord
	found, err := s.getJSON(ctx, kindSession, tokenKey(t), &sr)
	if err != nil || !found {
		return "", nil, false, err
	}
	m = &Member{}
	found, err = s.getJSON(ctx, kindMember, sr.ConnID, m)
	if err != nil {
		return "", nil, false, err
	}
	if !found {
		_ = s.Store.Delete(ctx, kindSession, tokenKey(t))
		return "", nil, false, nil
	}
	return sr.ConnID, m, true, nil
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	conn, m, ok, err := s.Member(r.Context(), r)
	if err != nil {
		s.fail(w, "store", err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "signed_out")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection_id": conn, "name": m.Name, "joined": m.Joined})
}

func (s *Server) signOut(w http.ResponseWriter, r *http.Request) {
	if t := cookie(r, SessionCookie); t != "" {
		if err := s.Store.Delete(r.Context(), kindSession, tokenKey(t)); err != nil {
			s.fail(w, "store", err)
			return
		}
	}
	setCookie(w, SessionCookie, "", 0)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// --- webhooks ---

// webhook acts on a verified event. An error makes the Receiver answer
// 500, so the service vault delivers the event again.
func (s *Server) webhook(ctx context.Context, e servicevault.Event) error {
	h := e.EventHeader()
	s.log().Info("webhook", "event_id", h.ID, "type", h.Type, "connection_id", h.ConnectionID)
	switch ev := e.(type) {
	case *servicevault.ConnectionPending:
		return s.updateFlow(ctx, ev.InviteID, func(f *flow) {
			if f.Stage == stageInvite {
				f.Stage, f.SAS = stageSAS, ev.SAS
			}
		})
	case *servicevault.ConnectionAdded:
		m := &Member{Name: ev.Name, Joined: h.CreatedAt.UTC(), EmailBound: ev.EmailBound != nil && *ev.EmailBound}
		// The name follows the event; a roster entry a session created
		// first keeps its join time.
		var old Member
		if ok, err := s.getJSON(ctx, kindMember, h.ConnectionID, &old); err != nil {
			return err
		} else if ok && !old.Joined.IsZero() {
			m.Joined = old.Joined
		}
		if _, removed, err := s.Store.Get(ctx, kindRemoved, h.ConnectionID); err != nil || removed {
			return err
		}
		if err := s.putJSON(ctx, kindMember, h.ConnectionID, m, time.Time{}, false); err != nil {
			return err
		}
		return s.updateFlow(ctx, ev.InviteID, func(f *flow) { f.Stage, f.ConnID = stageConnected, h.ConnectionID })
	case *servicevault.ConnectionDeclined:
		return s.updateFlow(ctx, ev.InviteID, func(f *flow) {
			if f.Stage != stageConnected {
				f.Stage = stageDeclined
			}
		})
	case *servicevault.ConnectionExpired:
		return s.updateFlow(ctx, ev.InviteID, func(f *flow) {
			if f.Stage != stageConnected {
				f.Stage = stageExpired
			}
		})
	case *servicevault.ConnectionRemoved:
		// Remembered, so that a flow still at connected cannot bring it
		// back; off the roster, and its sessions end at their next use
		// (Member).
		if err := s.Store.Put(ctx, kindRemoved, h.ConnectionID, nil, s.now().Add(RemovedTTL), false); err != nil {
			return err
		}
		return s.Store.Delete(ctx, kindMember, h.ConnectionID)
	case *servicevault.SignInApproved:
		if h.ConnectionID == "" {
			return nil
		}
		return s.updateSignIn(ctx, ev.SignInID, func(f *signIn) {
			f.Stage, f.ConnID, f.KeyChanged = signInApproved, h.ConnectionID, ev.KeyChanged
		})
	case *servicevault.SignInDenied:
		return s.updateSignIn(ctx, ev.SignInID, func(f *signIn) { f.Stage = signInDenied })
	case *servicevault.SignInExpired:
		return s.updateSignIn(ctx, ev.SignInID, func(f *signIn) { f.Stage = signInExpired })
	}
	return nil
}

// updateFlow applies change to the flow of an invitation, if any.
func (s *Server) updateFlow(ctx context.Context, inviteID string, change func(*flow)) error {
	fk, ok, err := s.Store.Get(ctx, kindInvite, inviteID)
	if err != nil || !ok {
		return err
	}
	mu := s.lock(string(fk))
	mu.Lock()
	defer mu.Unlock()
	f := &flow{}
	found, err := s.getJSON(ctx, kindFlow, string(fk), f)
	if err != nil || !found {
		return err
	}
	change(f)
	return s.putJSON(ctx, kindFlow, string(fk), f, f.Exp, false)
}

// replay is the webhook ReplayCache on the store, so a redelivery after a
// restart is still recognized.
type replay struct{ s store.Store }

func (r *replay) Add(ctx context.Context, id string, _, until time.Time) (bool, error) {
	err := r.s.Put(ctx, kindHook, id, nil, until, true)
	if errors.Is(err, store.ErrExists) {
		return false, nil
	}
	return err == nil, err
}

func (r *replay) Remove(ctx context.Context, id string) error { return r.s.Delete(ctx, kindHook, id) }

// --- errors ---

func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.log().Error("internal", "what", what, "err", errName(err))
	writeError(w, http.StatusInternalServerError, "internal")
}

// errName names an error for the log without its text, which may carry
// an address or a URL.
func errName(err error) string {
	var se *servicevault.APIError
	switch {
	case errors.As(err, &se):
		return "service_vault:" + se.Code
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	default:
		return "error"
	}
}
