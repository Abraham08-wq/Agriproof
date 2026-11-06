package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agriproof/backend/internal/auth"
	"github.com/agriproof/backend/internal/mpesa"
)

var ErrUnauthorized = errors.New("unauthorized")

const (
	otpTTL             = 5 * time.Minute
	otpMaxAttempts     = 5
	otpMaxPer15Minutes = 3
	otpMaxWrongPerDay  = 15 // wrong codes per phone in 24 hours before sign-in is locked for that phone
	phoneSessionTTL    = 30 * 24 * time.Hour
	adminSessionTTL    = 12 * time.Hour
	adminMaxFailures   = 5
	adminLockout       = 15 * time.Minute
)

// ---------- audit ----------

// Audit records who did what. Failures are logged, never returned, so auditing cannot block service.
func (s *Service) Audit(ctx context.Context, actorType, actor, action, target string, detail map[string]any, ip string) {
	if detail == nil {
		detail = map[string]any{}
	}
	d, _ := json.Marshal(detail)
	if _, err := s.db.Exec(ctx, `INSERT INTO audit_log(actor_type, actor, action, target, detail, ip) VALUES ($1,$2,$3,$4,$5,$6)`,
		actorType, actor, action, target, d, ip); err != nil {
		s.log.Error("audit write failed", "err", err, "action", action)
	}
}

type AuditEntry struct {
	ActorType string          `json:"actor_type"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Target    string          `json:"target"`
	Detail    json.RawMessage `json:"detail"`
	IP        string          `json:"ip"`
	CreatedAt time.Time       `json:"created_at"`
}

func (s *Service) AuditLog(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT actor_type, actor, action, target, detail, ip, created_at FROM audit_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ActorType, &a.Actor, &a.Action, &a.Target, &a.Detail, &a.IP, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------- member phone login ----------

// RequestOTP sends a sign-in code to a registered staff member or farmer.
func (s *Service) RequestOTP(ctx context.Context, phone string) error {
	p, err := mpesa.NormalizePhone(phone)
	if err != nil {
		return invalid("enter a valid phone number")
	}
	// Only registered staff and farmers get a code (no SMS money spent on unknown numbers).
	// The caller always gets the same answer, so this does not reveal who is registered.
	var known bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM staff WHERE phone=$1 AND active)
		OR EXISTS(SELECT 1 FROM farmers WHERE phone=$1 AND status IN ('active','held'))`, p).Scan(&known); err != nil {
		return err
	}
	if !known {
		s.log.Info("sign-in code requested for an unregistered number")
		return nil
	}
	var recent int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM otp_codes WHERE phone=$1 AND created_at > now() - interval '15 minutes'`, p).Scan(&recent); err != nil {
		return err
	}
	if locked, err := s.otpLocked(ctx, p); err != nil || locked {
		if err != nil {
			return err
		}
		// Same answer as for any other number, so limits do not reveal who is registered.
		s.log.Info("sign-in code not sent: number locked after wrong codes")
		return nil
	}
	if recent >= otpMaxPer15Minutes {
		s.log.Info("sign-in code not sent: too many requests for this number")
		return nil
	}
	code, err := auth.NewOTP()
	if err != nil {
		return err
	}
	// Send first, so a code is only stored (and guessable) if it really went out.
	if err := s.sms.SendCode(ctx, p, fmt.Sprintf("Your AgriProof code is %s. It expires in 5 minutes. Never share it with anyone, including cooperative staff.", code)); err != nil {
		s.log.Warn("sign-in code not sent", "err", err)
		return nil // same answer as for unregistered numbers
	}
	_, err = s.db.Exec(ctx, `INSERT INTO otp_codes(phone, code_hash, expires_at) VALUES ($1,$2,$3)`,
		p, auth.HashOTP(s.secret, p, code), time.Now().Add(otpTTL))
	return err
}

// VerifyOTP checks the newest unused code for the phone and starts a phone session.
func (s *Service) VerifyOTP(ctx context.Context, phone, code, device string) (string, error) {
	p, err := mpesa.NormalizePhone(phone)
	if err != nil {
		return "", invalid("enter a valid phone number")
	}
	code = strings.TrimSpace(code)
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var id int64
	var hash string
	var attempts int
	err = tx.QueryRow(ctx, `SELECT id, code_hash, attempts FROM otp_codes
		WHERE phone=$1 AND NOT used AND expires_at > now() ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, p).
		Scan(&id, &hash, &attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: the code has expired; request a new one", ErrUnauthorized)
	}
	if err != nil {
		return "", err
	}
	if attempts >= otpMaxAttempts {
		return "", fmt.Errorf("%w: too many wrong attempts; request a new code", ErrUnauthorized)
	}
	if locked, err := s.otpLocked(ctx, p); err != nil || locked {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: too many wrong codes for this number today; try again in 24 hours", ErrUnauthorized)
	}
	if !auth.EqualHex(hash, auth.HashOTP(s.secret, p, code)) {
		if _, err := tx.Exec(ctx, `UPDATE otp_codes SET attempts=attempts+1 WHERE id=$1`, id); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: wrong code", ErrUnauthorized)
	}
	if _, err := tx.Exec(ctx, `UPDATE otp_codes SET used=TRUE WHERE id=$1`, id); err != nil {
		return "", err
	}
	token, tokenHash, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	if len(device) > 120 {
		device = device[:120]
	}
	if _, err := tx.Exec(ctx, `INSERT INTO phone_sessions(token_hash, phone, device, expires_at) VALUES ($1,$2,$3,$4)`,
		tokenHash, p, device, time.Now().Add(phoneSessionTTL)); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return token, nil
}

// PhoneSession returns the phone number behind a session token.
func (s *Service) PhoneSession(ctx context.Context, token string) (string, error) {
	var phone string
	err := s.db.QueryRow(ctx, `UPDATE phone_sessions SET last_seen_at=now()
		WHERE token_hash=$1 AND expires_at > now() RETURNING phone`, auth.HashToken(token)).Scan(&phone)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return phone, err
}

func (s *Service) EndPhoneSession(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM phone_sessions WHERE token_hash=$1`, auth.HashToken(token))
	return err
}

// ---------- console admins ----------

type Admin struct {
	ID          string     `json:"id"`
	Username    string     `json:"username"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

func (s *Service) AdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// CreateAdmin stores a new admin and returns the authenticator secret to enrol.
func (s *Service) CreateAdmin(ctx context.Context, username, password string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", invalid("%v", err)
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO admins(username, password_hash, totp_secret) VALUES ($1,$2,$3)`, username, hash, secret)
	if isUniqueViolation(err) {
		return "", fmt.Errorf("%w: that username already exists", ErrConflict)
	}
	if err != nil {
		if strings.Contains(err.Error(), "admins_username_check") {
			return "", invalid("username must be 3-40 characters: lowercase letters, numbers, dot, dash or underscore")
		}
		return "", err
	}
	s.Audit(ctx, "system", "cli", "admin.created", username, nil, "")
	return secret, nil
}

// AdminLogin checks password and authenticator code. Five wrong codes after a right password lock the account for 15 minutes.
func (s *Service) AdminLogin(ctx context.Context, username, password, code, ip string) (string, Admin, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	fail := fmt.Errorf("%w: wrong username, password or code", ErrUnauthorized)
	var a Admin
	var hash, secret string
	var lastStep int64
	var failures int
	var lockedUntil *time.Time
	err := s.db.QueryRow(ctx, `SELECT id, username, password_hash, totp_secret, totp_last_step, failed_attempts, locked_until, created_at, last_login_at
		FROM admins WHERE username=$1`, username).
		Scan(&a.ID, &a.Username, &hash, &secret, &lastStep, &failures, &lockedUntil, &a.CreatedAt, &a.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Spend the same time as a real check so usernames cannot be discovered by timing.
		auth.CheckPassword("$argon2id$v=19$m=65536,t=3,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
		s.Audit(ctx, "admin", username, "admin.login_failed", "", map[string]any{"reason": "unknown user"}, ip)
		return "", a, fail
	}
	if err != nil {
		return "", a, err
	}
	if lockedUntil != nil && lockedUntil.After(time.Now()) {
		return "", a, fmt.Errorf("%w: account locked after repeated failures; try again in %d minutes", ErrUnauthorized,
			int(time.Until(*lockedUntil).Minutes())+1)
	}
	passOK := auth.CheckPassword(hash, password)
	step, codeOK := auth.VerifyTOTP(secret, code, time.Now(), lastStep)
	if !passOK || !codeOK {
		// Only a right password with a wrong code counts towards the lock. Otherwise anyone who knows
		// a username could keep the operator locked out by typing wrong passwords. Wrong passwords are
		// still slowed by the sign-in rate limit and the slow password hash, and are useless without the code.
		if !passOK {
			s.Audit(ctx, "admin", username, "admin.login_failed", "", map[string]any{"password_ok": false}, ip)
			return "", a, fail
		}
		failures++
		var lock *time.Time
		if failures >= adminMaxFailures {
			t := time.Now().Add(adminLockout)
			lock, failures = &t, 0
		}
		_, _ = s.db.Exec(ctx, `UPDATE admins SET failed_attempts=$2, locked_until=$3 WHERE id=$1`, a.ID, failures, lock)
		s.Audit(ctx, "admin", username, "admin.login_failed", "", map[string]any{"password_ok": passOK, "code_ok": codeOK}, ip)
		return "", a, fail
	}
	token, tokenHash, err := auth.NewToken()
	if err != nil {
		return "", a, err
	}
	// The condition makes each authenticator code usable once, even for two sign-ins at the same moment.
	tag, err := s.db.Exec(ctx, `UPDATE admins SET failed_attempts=0, locked_until=NULL, totp_last_step=$2, last_login_at=now()
		WHERE id=$1 AND totp_last_step < $2`, a.ID, step)
	if err != nil {
		return "", a, err
	}
	if tag.RowsAffected() != 1 {
		s.Audit(ctx, "admin", username, "admin.login_failed", "", map[string]any{"reason": "code already used"}, ip)
		return "", a, fail
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO admin_sessions(token_hash, admin_id, expires_at) VALUES ($1,$2,$3)`,
		tokenHash, a.ID, time.Now().Add(adminSessionTTL)); err != nil {
		return "", a, err
	}
	s.Audit(ctx, "admin", username, "admin.login", "", nil, ip)
	return token, a, nil
}

func (s *Service) AdminSession(ctx context.Context, token string) (Admin, error) {
	var a Admin
	err := s.db.QueryRow(ctx, `SELECT a.id, a.username, a.created_at, a.last_login_at FROM admin_sessions s
		JOIN admins a ON a.id=s.admin_id WHERE s.token_hash=$1 AND s.expires_at > now()`, auth.HashToken(token)).
		Scan(&a.ID, &a.Username, &a.CreatedAt, &a.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrUnauthorized
	}
	return a, err
}

func (s *Service) AdminLogout(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash=$1`, auth.HashToken(token))
	return err
}

// PurgeExpired removes expired codes and sessions. Called periodically.
func (s *Service) PurgeExpired(ctx context.Context) {
	_, _ = s.db.Exec(ctx, `DELETE FROM otp_codes WHERE expires_at < now() - interval '1 day'`)
	_, _ = s.db.Exec(ctx, `DELETE FROM phone_sessions WHERE expires_at < now()`)
	_, _ = s.db.Exec(ctx, `DELETE FROM admin_sessions WHERE expires_at < now()`)
	s.expireStale(ctx)
}

// otpLocked is true when a phone has had too many wrong codes in the last 24 hours.
func (s *Service) otpLocked(ctx context.Context, phone string) (bool, error) {
	var wrong int
	err := s.db.QueryRow(ctx, `SELECT COALESCE(SUM(attempts),0) FROM otp_codes WHERE phone=$1 AND created_at > now() - interval '24 hours'`, phone).Scan(&wrong)
	return wrong >= otpMaxWrongPerDay, err
}
