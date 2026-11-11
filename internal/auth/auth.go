// Package auth handles password hashing and the two token types.
//
// Access token: short lived JWT (15m by default) carrying the user id and the
// session id. It deliberately does NOT carry the role, because the brief wants a
// role change or a removed membership to bite on the very next request; the
// middleware re-reads the session row and the user row on every call.
//
// Refresh token: opaque 32 random bytes, only its SHA-256 stored. Rotated on
// every refresh so a stolen refresh token is usable at most once.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	RoleUser         = "user"
	RoleStoreManager = "store_manager"
	RoleSuperAdmin   = "superadmin"
)

func ValidRole(role string) bool {
	return role == RoleUser || role == RoleStoreManager || role == RoleSuperAdmin
}

func HashPassword(plain string, cost int) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	return string(b), err
}

func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

type Claims struct {
	SessionID string `json:"sid"`
	UserID    string `json:"sub"`
	jwt.RegisteredClaims
}

type Tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

func NewTokens(secret []byte, ttl time.Duration) (Tokens, error) {
	refreshBytes := make([]byte, 32)
	if _, err := rand.Read(refreshBytes); err != nil {
		return Tokens{}, err
	}
	return Tokens{
		RefreshToken: base64.RawURLEncoding.EncodeToString(refreshBytes),
		ExpiresIn:    int(ttl.Seconds()),
	}, nil
}

// SignAccess builds the JWT for a session. Called after the session row exists.
func SignAccess(secret []byte, ttl time.Duration, userID, sessionID string) (string, error) {
	now := time.Now()
	claims := Claims{
		SessionID: sessionID,
		UserID:    userID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-30 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "sitetrack",
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

func ParseAccess(secret []byte, raw string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("sitetrack"))
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

// HashRefresh is what goes into the session row. Refresh tokens are high entropy
// random values, so a fast hash is enough; bcrypt here would just add latency.
func HashRefresh(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
