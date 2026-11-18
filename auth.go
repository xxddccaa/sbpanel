package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	passwordIterations = 300_000
	sessionLifetime    = 30 * 24 * time.Hour
	minPasswordLength  = 8
)

type authData struct {
	PasswordHash string `json:"passwordHash"`
	SessionKey   string `json:"sessionKey"`
}

// Auth lives in its own file so `sbpanel passwd` can change it while the panel runs.
type Auth struct {
	path    string
	mu      sync.Mutex
	data    authData
	modTime time.Time
}

func NewAuth(path string) *Auth { return &Auth{path: path} }

func (a *Auth) current() (authData, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	info, err := os.Stat(a.path)
	if err != nil {
		return authData{}, err
	}
	if !info.ModTime().Equal(a.modTime) {
		raw, err := os.ReadFile(a.path)
		if err != nil {
			return authData{}, err
		}
		var data authData
		if err := json.Unmarshal(raw, &data); err != nil {
			return authData{}, fmt.Errorf("parse %s: %w", a.path, err)
		}
		a.data = data
		a.modTime = info.ModTime()
	}
	return a.data, nil
}

func (a *Auth) Exists() bool {
	_, err := os.Stat(a.path)
	return !errors.Is(err, fs.ErrNotExist)
}

// SetPassword also rotates the session key, which signs everyone out.
func (a *Auth) SetPassword(password string) error {
	if len(password) < minPasswordLength {
		return fmt.Errorf("密码至少 %d 位", minPasswordLength)
	}
	data := authData{
		PasswordHash: hashPassword(password),
		SessionKey:   base64.RawStdEncoding.EncodeToString(randomBytes(32)),
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := writeFileAtomic(a.path, raw, 0o600); err != nil {
		return err
	}
	a.modTime = time.Time{}
	return nil
}

func (a *Auth) Verify(password string) bool {
	data, err := a.current()
	if err != nil {
		return false
	}
	return verifyPassword(data.PasswordHash, password)
}

func (a *Auth) IssueSession(now time.Time) (string, error) {
	data, err := a.current()
	if err != nil {
		return "", err
	}
	expiry := strconv.FormatInt(now.Add(sessionLifetime).Unix(), 10)
	return expiry + "." + sessionMAC(data.SessionKey, expiry), nil
}

func (a *Auth) CheckSession(token string, now time.Time) bool {
	data, err := a.current()
	if err != nil {
		return false
	}
	expiry, mac, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	expected := sessionMAC(data.SessionKey, expiry)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(expected)) != 1 {
		return false
	}
	seconds, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && now.Unix() < seconds
}

func sessionMAC(key, expiry string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("sbpanel-session|" + expiry))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func hashPassword(password string) string {
	salt := randomBytes(16)
	derived := pbkdf2SHA256([]byte(password), salt, passwordIterations, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", passwordIterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(derived))
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	derived := pbkdf2SHA256([]byte(password), salt, iterations, len(expected))
	return subtle.ConstantTimeCompare(derived, expected) == 1
}

// pbkdf2SHA256 implements RFC 8018 PBKDF2 with HMAC-SHA256 (the standard library
// only gained crypto/pbkdf2 in Go 1.24).
func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLength := prf.Size()
	blocks := (keyLength + hashLength - 1) / hashLength
	derived := make([]byte, 0, blocks*hashLength)
	counter := make([]byte, 4)
	u := make([]byte, hashLength)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(counter, uint32(block))
		prf.Write(counter)
		derived = prf.Sum(derived)
		t := derived[len(derived)-hashLength:]
		copy(u, t)
		for n := 2; n <= iterations; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for i := range u {
				t[i] ^= u[i]
			}
		}
	}
	return derived[:keyLength]
}

func randomBytes(n int) []byte {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return buffer
}

// randomPassword avoids look-alike characters so it can be read off a screen and typed.
func randomPassword(length int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	raw := randomBytes(length)
	result := make([]byte, length)
	for i, b := range raw {
		result[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(result)
}

// loginLimiter slows down password guessing per client address.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string]*loginFailures
}

type loginFailures struct {
	count        int
	blockedUntil time.Time
}

const (
	loginFailuresBeforeBlock = 5
	loginBlockDuration       = time.Minute
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string]*loginFailures{}}
}

func (l *loginLimiter) blockedFor(client string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.failures[client]
	if entry == nil || !now.Before(entry.blockedUntil) {
		return 0
	}
	return entry.blockedUntil.Sub(now)
}

func (l *loginLimiter) record(client string, success bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if success {
		delete(l.failures, client)
		return
	}
	entry := l.failures[client]
	if entry == nil {
		entry = &loginFailures{}
		l.failures[client] = entry
	}
	entry.count++
	if entry.count%loginFailuresBeforeBlock == 0 {
		entry.blockedUntil = now.Add(loginBlockDuration)
	}
}
