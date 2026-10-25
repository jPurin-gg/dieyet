package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashVerify(t *testing.T) {
	h := Hash("ひみつ1234")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC string %q", h)
	}
	if !Verify("ひみつ1234", h) {
		t.Fatal("correct passphrase rejected")
	}
	if Verify("ひみつ1235", h) {
		t.Fatal("wrong passphrase accepted")
	}
	if Hash("ひみつ1234") == h {
		t.Fatal("salt not random")
	}
	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0c2E$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=999999999,t=2,p=1$c2FsdHNhbHRzYWx0c2E$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$$",
	} {
		if Verify("ひみつ1234", bad) {
			t.Fatalf("accepted malformed hash %q", bad)
		}
	}
}

func TestNewTokenUnique(t *testing.T) {
	if a, b := NewToken(), NewToken(); a == b || len(a) < 40 {
		t.Fatalf("bad tokens %q %q", a, b)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("attempt %d denied", i+1)
		}
	}
	if l.Allow("a") {
		t.Fatal("4th attempt allowed")
	}
	if !l.Allow("b") {
		t.Fatal("other key denied")
	}
	now = now.Add(61 * time.Second)
	if !l.Allow("a") {
		t.Fatal("not allowed after window passed")
	}
}
