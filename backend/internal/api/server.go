// Package api is AgriProof's web layer: the console, the field app, public check pages, and the
// callbacks from the SMS and payment companies.
package api

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agriproof/backend/internal/core"
	"github.com/agriproof/backend/internal/mpesa"
)

type Server struct {
	svc        *core.Service
	apiKey     string
	trustProxy bool
	webhookKey string // IntaSend webhook challenge
	monitorKey string // read-only key for the watchman
	inboundKey string // secret part of the incoming-SMS callback URL
	publicURL  string
	log        *slog.Logger

	loginLimit  *limiter
	otpLimit    *limiter
	verifyLimit *limiter
	checkLimit  *limiter // public bag-code checks per connection
}

func NewServer(svc *core.Service, apiKey string, trustProxy bool, webhookKey string, log *slog.Logger) *Server {
	return &Server{
		svc: svc, apiKey: apiKey, trustProxy: trustProxy, webhookKey: webhookKey, log: log,
		loginLimit:  newLimiter(10, time.Minute),
		otpLimit:    newLimiter(5, time.Minute),
		verifyLimit: newLimiter(15, time.Minute),
		checkLimit:  newLimiter(20, time.Minute),
	}
}

// SetKeys sets the watchman's read-only key, the incoming-SMS secret and the public address.
func (s *Server) SetKeys(monitorKey, inboundKey, publicURL string) {
	s.monitorKey, s.inboundKey, s.publicURL = monitorKey, inboundKey, strings.TrimRight(publicURL, "/")
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Pages
	mux.HandleFunc("GET /{$}", page(consoleHTML, ""))
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /app/{$}", page(appHTML, "manifest-src 'self'; worker-src 'self'; "))
	mux.HandleFunc("GET /check", page(checkHTML, ""))
	mux.HandleFunc("GET /check/{code}", page(checkHTML, ""))
	mux.HandleFunc("GET /p/{code}", page(lotHTML, ""))
	mux.HandleFunc("GET /brand/{name}", serveBrand)
	staticFiles(mux)

	// Public, no sign-in
	mux.HandleFunc("POST /public/check", s.limit(s.checkLimit, s.publicCheck))
	mux.HandleFunc("GET /public/lots/{code}", s.limit(s.checkLimit, func(w http.ResponseWriter, r *http.Request) {
		p, err := s.svc.LotProofByCode(r.Context(), r.PathValue("code"))
		s.respond(w, http.StatusOK, p, err)
	}))
	mux.HandleFunc("POST /sms/inbound/{key}", s.inboundSMS)
	mux.HandleFunc("POST /payments/intasend/webhook", s.intasendWebhook)
	mux.HandleFunc("POST /mpesa/callback", s.mpesaCallback)

	// Sign-in
	mux.HandleFunc("GET /admin/status", s.adminStatus)
	mux.HandleFunc("POST /admin/login", s.limit(s.loginLimit, s.adminLogin))
	mux.HandleFunc("POST /admin/logout", s.adminLogout)
	mux.HandleFunc("POST /auth/otp/request", s.limit(s.otpLimit, s.otpRequest))
	mux.HandleFunc("POST /auth/otp/verify", s.limit(s.verifyLimit, s.otpVerify))

	// Field app (signed in with a phone)
	field := s.requireMember(s.fieldRoutes())
	mux.Handle("/me", field)
	mux.Handle("/me/", field)

	// Console (signed-in operator or the service key)
	mux.Handle("/", s.requireAdmin(s.consoleRoutes()))
	return s.logger(securityHeaders(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(self)")
		next.ServeHTTP(w, r)
	})
}

// ---------- pages ----------

//go:embed web
var webFiles embed.FS

func mustRead(name string) []byte {
	b, err := webFiles.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	consoleHTML = mustRead("web/console.html")
	appHTML     = mustRead("web/app.html")
	checkHTML   = mustRead("web/check.html")
	lotHTML     = mustRead("web/lot.html")
	sharedCSS   = mustRead("web/theme.css")
)

// page serves an HTML page with a fresh nonce, so only its own script and styles run.
func page(html []byte, extraCSP string) http.HandlerFunc {
	full := bytes.ReplaceAll(html, []byte("{{THEME}}"), sharedCSS)
	return func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 18)
		_, _ = rand.Read(b)
		nonce := base64.StdEncoding.EncodeToString(b)
		out := bytes.ReplaceAll(full, []byte("{{NONCE}}"), []byte(nonce))
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"' https://fonts.googleapis.com; "+
				"font-src https://fonts.gstatic.com; connect-src 'self'; img-src 'self' data: https://images.unsplash.com; "+extraCSP+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		_, _ = w.Write(out)
	}
}

func staticFiles(mux *http.ServeMux) {
	files := map[string]string{
		"/app/manifest.webmanifest": "application/manifest+json",
		"/app/sw.js":                "text/javascript; charset=utf-8",
		"/app/icon.svg":             "image/svg+xml",
	}
	for path, ctype := range files {
		data := mustRead("web/" + path[len("/app/"):])
		ct := ctype
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			if strings.HasSuffix(path, "sw.js") {
				w.Header().Set("Service-Worker-Allowed", "/app/")
			}
			_, _ = w.Write(data)
		})
	}
}

// Farm photos, free under the Unsplash License. make setup saves local copies in brand/;
// until then the page loads them from Unsplash.
var brandPhotos = map[string]string{
	"hero.jpg":     "https://images.unsplash.com/photo-1758164280826-b7368e8109b5?fm=jpg&q=70&w=2000&auto=format&fit=crop",
	"seedling.jpg": "https://images.unsplash.com/photo-1542601906990-b4d3fb778b09?fm=jpg&q=70&w=1600&auto=format&fit=crop",
	"rows.jpg":     "https://images.unsplash.com/photo-1788122290883-6587385ffb8c?fm=jpg&q=70&w=1600&auto=format&fit=crop",
	"field.jpg":    "https://images.unsplash.com/photo-1782852588113-ff57cf895c98?fm=jpg&q=70&w=2000&auto=format&fit=crop",
}

func serveBrand(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	remote, ok := brandPhotos[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	dir := os.Getenv("BRAND_DIR")
	if dir == "" {
		dir = "/brand"
	}
	if f, err := os.Open(filepath.Join(dir, name)); err == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		_, _ = io.Copy(w, f)
		return
	}
	http.Redirect(w, r, remote, http.StatusFound)
}

// ---------- public bag-code check ----------

func (s *Server) publicCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := s.svc.CheckCode(r.Context(), in.Code, "", "web", s.clientIP(r))
	s.respond(w, http.StatusOK, res, err)
}

// inboundSMS answers farmers who text a bag code (Africa's Talking incoming messages). The callback URL
// carries a secret so nobody else can post to it.
func (s *Server) inboundSMS(w http.ResponseWriter, r *http.Request) {
	if len(s.inboundKey) < 16 || subtle.ConstantTimeCompare([]byte(r.PathValue("key")), []byte(s.inboundKey)) != 1 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	from, text := r.FormValue("from"), r.FormValue("text")
	phone, err := mpesa.NormalizePhone(from)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	if !s.checkLimit.allow("sms:" + phone) {
		w.WriteHeader(http.StatusOK)
		return
	}
	res, err := s.svc.CheckCode(r.Context(), text, phone, "sms", "")
	reply := ""
	if err != nil {
		reply = "AgriProof: send the 12-character code from the bag label, like ABCD-EFGH-JKMN."
	} else {
		reply = "AgriProof: " + res.Headline + ". " + res.Message
		if len(res.Warnings) > 0 {
			reply += " Note: " + strings.Join(res.Warnings, "; ") + "."
		}
	}
	s.svc.SendSMS(phone, reply)
	w.WriteHeader(http.StatusOK)
}

// ---------- payment notifications: hints only, always confirmed with the payment company ----------

func (s *Server) intasendWebhook(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Challenge  string `json:"challenge"`
		InvoiceID  string `json:"invoice_id"`
		TrackingID string `json:"tracking_id"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if s.webhookKey == "" || subtle.ConstantTimeCompare([]byte(in.Challenge), []byte(s.webhookKey)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if in.InvoiceID != "" {
		if err := s.svc.VerifyFunding(r.Context(), in.InvoiceID); err != nil {
			s.log.Warn("funding check after webhook failed", "err", err)
		}
	}
	if in.TrackingID != "" {
		s.svc.WakePayments()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

func (s *Server) mpesaCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err == nil {
		if cb, err := mpesa.ParseCallback(body); err == nil {
			if err := s.svc.VerifyFunding(r.Context(), cb.CheckoutRequestID); err != nil {
				s.log.Warn("funding check after callback failed", "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ResultCode": 0, "ResultDesc": "Accepted"})
}

// ---------- helpers ----------

func (s *Server) respond(w http.ResponseWriter, okStatus int, v any, err error) {
	if err == nil {
		writeJSON(w, okStatus, v)
		return
	}
	status, msg := errStatus(err)
	if status == http.StatusInternalServerError {
		s.log.Error("request failed", "err", err)
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func isErr(err, target error) bool { return errors.Is(err, target) }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func qInt(r *http.Request, key string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && n > 0 {
		return n
	}
	return def
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.Method == http.MethodGet && rec.status < 400 {
			return
		}
		path := r.URL.Path
		if strings.HasPrefix(path, "/sms/inbound/") {
			path = "/sms/inbound/…" // keep the secret out of logs
		}
		s.log.Info("http", "method", r.Method, "path", path, "status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

var _ = fmt.Sprintf
