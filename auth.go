package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pbkdfIter   = 210000
	tokenTTL    = 90 * 24 * time.Hour
	maxTokens   = 30
	maxFailsMin = 10
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdfIter, 32)
	return fmt.Sprintf("pbkdf2$%d$%s$%s", pbkdfIter, hex.EncodeToString(salt), hex.EncodeToString(key))
}

func checkPassword(stored, pw string) bool {
	p := strings.Split(stored, "$")
	if len(p) != 4 || p[0] != "pbkdf2" {
		return false
	}
	iter, err := strconv.Atoi(p[1])
	salt, err2 := hex.DecodeString(p[2])
	want, err3 := hex.DecodeString(p[3])
	if err != nil || err2 != nil || err3 != nil || iter < 1 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func hashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}

// loginLimiter is a global brute-force brake. It is global rather than per-IP
// because behind a tunnel/reverse proxy every client shares one address.
type loginLimiter struct {
	mu    sync.Mutex
	fails []time.Time
}

func (l *loginLimiter) blocked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	i := 0
	for i < len(l.fails) && l.fails[i].Before(cut) {
		i++
	}
	l.fails = l.fails[i:]
	return len(l.fails) >= maxFailsMin
}

func (l *loginLimiter) fail() {
	l.mu.Lock()
	l.fails = append(l.fails, time.Now())
	l.mu.Unlock()
}
