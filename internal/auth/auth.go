// Package auth hashes passphrases, mints session tokens and rate-limits login attempts.
//
// Passphrase hashes are stored as PHC strings
// ($argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>, base64 without padding) so
// that the workout app, which shares the users table, can verify and create
// them with any argon2 library.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters for new hashes: the OWASP minimum (19 MiB, 2 passes,
// 1 lane). A login verifies one hash per account sharing the nickname, so
// keep this modest.
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 19 * 1024
	argonThreads uint8  = 1
	keyLen       uint32 = 32
	saltLen             = 16
	maxMemory    uint32 = 256 * 1024 // refuse stored hashes that would ask for more
)

var b64 = base64.RawStdEncoding

// Hash returns the argon2id hash of passphrase as a PHC string with a fresh salt.
func Hash(passphrase string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt) // never fails on supported platforms (Go 1.24+)
	key := argon2.IDKey([]byte(passphrase), salt, argonTime, argonMemory, argonThreads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// Verify reports whether passphrase matches the PHC string, in constant time.
func Verify(passphrase, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	if m == 0 || m > maxMemory || t == 0 || t > 16 || p == 0 {
		return false
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 128 {
		return false
	}
	got := argon2.IDKey([]byte(passphrase), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// NewToken returns 256 bits of randomness as a URL-safe string (session ids).
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Limiter allows at most limit events per key within a sliding window.
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	hits map[string][]time.Time
}

func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

// Allow records an event for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 { // crude memory bound: forget everything idle
		for k, ts := range l.hits {
			if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
				delete(l.hits, k)
			}
		}
	}
	ts := l.hits[key]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
