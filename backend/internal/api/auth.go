package api

import (
	"context"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agriproof/backend/internal/core"
	"github.com/agriproof/backend/internal/mpesa"
)

type ctxKey int

const (
	ctxActor ctxKey = iota
	ctxPhone
)

// Actor is who is calling an admin endpoint.
type Actor struct {
	Type string `json:"type"` // admin | service | member
	Name string `json:"name"`
}

func actorFrom(r *http.Request) Actor {
	if a, ok := r.Context().Value(ctxActor).(Actor); ok {
		return a
	}
	return Actor{Type: "service", Name: "unknown"}
}

func phoneFrom(r *http.Request) string {
	p, _ := r.Context().Value(ctxPhone).(string)
	return p
}

const adminCookie = "ap_admin"

// requireAdmin accepts either the service access key (for scripts and integrations) or
// a signed-in admin's session cookie. Cookie-authenticated changes must also carry the
// X-Requested-With header, which other websites cannot send (CSRF protection).
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("X-API-Key"); key != "" {
			// The watchman has its own key that can only read.
			if len(s.monitorKey) >= 24 && subtle.ConstantTimeCompare([]byte(key), []byte(s.monitorKey)) == 1 {
				if r.Method != http.MethodGet {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "the monitor key can only read"})
					return
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxActor, Actor{Type: "service", Name: "monitor"})))
				return
			}
			if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid access key"})
				return
			}
			s.serveAudited(w, r, next, Actor{Type: "service", Name: "access-key"})
			return
		}
		c, err := r.Cookie(adminCookie)
		if err != nil || c.Value == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
			return
		}
		admin, err := s.svc.AdminSession(r.Context(), c.Value)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "your session has ended; sign in again"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "agriproof" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing request header"})
			return
		}
		s.serveAudited(w, r, next, Actor{Type: "admin", Name: admin.Username})
	})
}

const memberCookie = "ap_session"

// memberToken reads the member session from the Android app's Bearer header or the web app's
// HttpOnly cookie. Cookie sessions must send X-Requested-With on changes (CSRF protection).
func memberToken(r *http.Request) (token string, viaCookie bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer "), false
	}
	if c, err := r.Cookie(memberCookie); err == nil {
		return c.Value, true
	}
	return "", false
}

// requireMember accepts a member session (Android app Bearer token or web app cookie).
func (s *Server) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, viaCookie := memberToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in required"})
			return
		}
		if viaCookie && r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "agriproof" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "missing request header"})
			return
		}
		phone, err := s.svc.PhoneSession(r.Context(), token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "your session has ended; sign in again"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxPhone, phone)
		s.serveAudited(w, r.WithContext(ctx), next, Actor{Type: "phone", Name: phone})
	})
}

// serveAudited runs the handler and records every successful change in the audit log.
func (s *Server) serveAudited(w http.ResponseWriter, r *http.Request, next http.Handler, a Actor) {
	r = r.WithContext(context.WithValue(r.Context(), ctxActor, a))
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(rec, r)
	if r.Method != http.MethodGet && rec.status < 400 {
		s.svc.Audit(r.Context(), a.Type, a.Name, r.Method+" "+r.URL.Path, r.PathValue("id"), nil, s.clientIP(r))
	}
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.trustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		// Use the address our own proxy added (the last one). Earlier entries come from the
		// client and could be invented to dodge rate limits.
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- rate limiting ----------

// limiter allows `max` requests per key in each window. It keeps state in memory,
// which is right for a single API instance.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*bucket
}

type bucket struct {
	count int
	reset time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	l := &limiter{max: max, window: window, hits: map[string]*bucket{}}
	go func() {
		for range time.Tick(window) {
			l.mu.Lock()
			now := time.Now()
			for k, b := range l.hits {
				if now.After(b.reset) {
					delete(l.hits, k)
				}
			}
			l.mu.Unlock()
		}
	}()
	return l
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.hits[key]
	if !ok || now.After(b.reset) {
		l.hits[key] = &bucket{count: 1, reset: now.Add(l.window)}
		return true
	}
	if b.count >= l.max {
		return false
	}
	b.count++
	return true
}

func (s *Server) limit(l *limiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(s.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts; wait a minute and try again"})
			return
		}
		next(w, r)
	}
}

// ---------- admin sign-in ----------

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.svc.AdminCount(r.Context())
	s.respond(w, http.StatusOK, map[string]bool{"admin_exists": n > 0}, err)
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	token, admin, err := s.svc.AdminLogin(r.Context(), in.Username, in.Password, in.Code, s.clientIP(r))
	if err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.isHTTPS(r),
		MaxAge: int((12 * time.Hour).Seconds()),
	})
	writeJSON(w, http.StatusOK, admin)
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(adminCookie); err == nil {
		_ = s.svc.AdminLogout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// ---------- member sign-in ----------

func (s *Server) otpRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.svc.RequestOTP(r.Context(), in.Phone); err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"message":   "If this number is registered with a cooperative, a 6-digit code is on its way by SMS.",
		"test_mode": s.svc.SMSTestMode(),
	})
}

func (s *Server) otpVerify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone  string `json:"phone"`
		Code   string `json:"code"`
		Device string `json:"device"`
	}
	if !decode(w, r, &in) {
		return
	}
	token, err := s.svc.VerifyOTP(r.Context(), in.Phone, in.Code, in.Device)
	if err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	phone, _ := mpesa.NormalizePhone(in.Phone)
	s.svc.Audit(r.Context(), "phone", phone, "phone.login", "", map[string]any{"device": in.Device}, s.clientIP(r))
	// The web app keeps its session in an HttpOnly cookie that page scripts cannot read.
	if r.Header.Get("X-Client") == "web" {
		http.SetCookie(w, &http.Cookie{
			Name: memberCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
			Secure: s.isHTTPS(r), MaxAge: int((30 * 24 * time.Hour).Seconds()),
		})
		writeJSON(w, http.StatusOK, map[string]bool{"signed_in": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

func errStatus(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case isErr(err, core.ErrValidation):
		return http.StatusBadRequest, err.Error()
	case isErr(err, core.ErrUnauthorized):
		return http.StatusUnauthorized, err.Error()
	case isErr(err, core.ErrNotFound):
		return http.StatusNotFound, err.Error()
	case isErr(err, core.ErrConflict):
		return http.StatusConflict, err.Error()
	case isErr(err, core.ErrForbidden):
		return http.StatusForbidden, err.Error()
	case isErr(err, core.ErrUnavailable):
		return http.StatusServiceUnavailable, err.Error()
	}
	return http.StatusInternalServerError, "internal error"
}
