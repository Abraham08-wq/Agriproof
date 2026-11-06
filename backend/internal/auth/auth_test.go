package auth

import (
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Fatal("valid password rejected")
	}
	if CheckPassword(h, "correct horse batterY") {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

// RFC 6238 test vector (SHA1, secret "12345678901234567890", T=59 -> 94287082, last 6 digits 287082).
func TestTOTPVector(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	code, err := TOTPCode(secret, 59/30)
	if err != nil || code != "287082" {
		t.Fatalf("got %s %v", code, err)
	}
	step, ok := VerifyTOTP(secret, "287082", time.Unix(59, 0), 0)
	if !ok || step != 1 {
		t.Fatal("valid code rejected")
	}
	if _, ok := VerifyTOTP(secret, "287082", time.Unix(59, 0), 1); ok {
		t.Fatal("reused code accepted")
	}
}

func TestOTPHashing(t *testing.T) {
	code, err := NewOTP()
	if err != nil || len(code) != 6 {
		t.Fatalf("bad code %q", code)
	}
	a := HashOTP([]byte("s1"), "254700000000", code)
	if !EqualHex(a, HashOTP([]byte("s1"), "254700000000", code)) {
		t.Fatal("hash not stable")
	}
	if EqualHex(a, HashOTP([]byte("s2"), "254700000000", code)) || EqualHex(a, HashOTP([]byte("s1"), "254700000001", code)) {
		t.Fatal("hash must depend on secret and phone")
	}
}

func TestTokens(t *testing.T) {
	tok, h, err := NewToken()
	if err != nil || len(tok) < 40 || HashToken(tok) != h {
		t.Fatal("token generation broken")
	}
}
