// Package config loads runtime settings from the environment.
//
// Everything has a usable default so that `docker compose up` needs no .env
// file; the compose file only sets DATABASE_URL and JWT_SECRET.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Addr            string
	DatabaseURL     string
	JWTSecret       []byte
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
	BcryptCost      int
	// MaxPageSize caps limit= on every list endpoint.
	MaxPageSize int
}

func Load() (Config, error) {
	cfg := Config{
		Addr:            env("ADDR", ":8080"),
		DatabaseURL:     env("DATABASE_URL", "postgres://sitetrack:sitetrack@localhost:5432/sitetrack?sslmode=disable"),
		JWTSecret:       []byte(env("JWT_SECRET", "")),
		AccessTokenTTL:  durationEnv("ACCESS_TOKEN_TTL", 15*time.Minute),
		RefreshTokenTTL: durationEnv("REFRESH_TOKEN_TTL", 30*24*time.Hour),
		BcryptCost:      intEnv("BCRYPT_COST", 12),
		MaxPageSize:     intEnv("MAX_PAGE_SIZE", 100),
	}
	if len(cfg.JWTSecret) == 0 {
		return cfg, fmt.Errorf("JWT_SECRET must be set")
	}
	if len(cfg.JWTSecret) < 16 {
		return cfg, fmt.Errorf("JWT_SECRET must be at least 16 bytes")
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func durationEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
