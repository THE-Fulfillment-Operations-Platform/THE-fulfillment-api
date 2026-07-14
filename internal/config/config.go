// Package config loads runtime configuration from environment variables (.env).
// Nothing about the database connection or secrets is hard-coded; everything is
// read from the environment so the same binary runs in any environment.
package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Well-known insecure defaults. Fine for local dev; must never reach production.
const (
	DefaultJWTSecret    = "change-me-in-production"
	DefaultDemoPassword = "Password123!"
	// Minimum acceptable JWT signing-key length in production. A short/guessable
	// key lets anyone forge tokens for any user, including OWNER.
	MinJWTSecretLen = 32
)

// Config holds all runtime configuration for the API server.
type Config struct {
	AppEnv  string
	AppName string
	Port    string

	// Database
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string
	DBTimeZone string

	// Auth
	JWTSecret    string
	JWTExpiresIn time.Duration

	// CORS
	CORSAllowedOrigins []string

	// Trusted reverse-proxy CIDRs/IPs. Empty = trust none, so ClientIP() derives
	// from the real TCP peer (RemoteAddr) and a spoofed X-Forwarded-For cannot
	// bypass IP-based rate limiting. Set to your LB/proxy range when deployed
	// behind one so the real client IP is honored.
	TrustedProxies []string

	// Seed
	SeedOnStart    bool
	SeedDemoUsers  bool
	SeedDemoOrders bool
	DemoPassword   string

	// Ops / danger zone
	AllowDataReset bool

	// Maintenance: periodically hard-delete rows soft-deleted longer ago than the
	// retention window, so GORM soft-deletes don't accumulate without bound.
	PurgeEnabled       bool
	PurgeRetentionDays int
	PurgeInterval      time.Duration
}

// Load reads configuration from a .env file (if present) and the process
// environment. Environment variables always win over the .env file.
func Load() *Config {
	// Best-effort: a missing .env is fine in production where real env vars are set.
	if err := godotenv.Load(); err != nil {
		log.Println("config: no .env file found, relying on process environment")
	}

	cfg := &Config{
		AppEnv:  getEnv("APP_ENV", "development"),
		AppName: getEnv("APP_NAME", "THE Fulfillment API"),
		Port:    getEnv("PORT", "8080"),

		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnv("DB_PORT", "5432"),
		DBUser:     getEnv("DB_USER", "postgres"),
		DBPassword: getEnv("DB_PASSWORD", "postgres"),
		DBName:     getEnv("DB_NAME", "the_fulfillment"),
		DBSSLMode:  getEnv("DB_SSLMODE", "disable"),
		DBTimeZone: getEnv("DB_TIMEZONE", "Asia/Ho_Chi_Minh"),

		JWTSecret:    getEnv("JWT_SECRET", "change-me-in-production"),
		JWTExpiresIn: time.Duration(getEnvAsInt("JWT_EXPIRES_HOURS", 72)) * time.Hour,

		CORSAllowedOrigins: splitAndTrim(getEnv("CORS_ALLOWED_ORIGINS", "*")),

		TrustedProxies: splitAndTrim(getEnv("TRUSTED_PROXIES", "")),

		SeedOnStart:    getEnvAsBool("SEED_ON_START", true),
		SeedDemoUsers:  getEnvAsBool("SEED_DEMO_USERS", true),
		SeedDemoOrders: getEnvAsBool("SEED_DEMO_ORDERS", false),
		DemoPassword:   getEnv("SEED_DEMO_PASSWORD", DefaultDemoPassword),

		AllowDataReset: getEnvAsBool("ALLOW_DATA_RESET", false),

		PurgeEnabled:       getEnvAsBool("PURGE_ENABLED", true),
		PurgeRetentionDays: getEnvAsInt("PURGE_RETENTION_DAYS", 30),
		PurgeInterval:      time.Duration(getEnvAsInt("PURGE_INTERVAL_HOURS", 24)) * time.Hour,
	}

	// Guard the purge window: a non-positive retention would hard-delete every
	// soft-deleted row immediately, so a misconfigured value falls back to 30 days.
	if cfg.PurgeRetentionDays <= 0 {
		log.Printf("config: PURGE_RETENTION_DAYS=%d invalid (must be > 0); using 30", cfg.PurgeRetentionDays)
		cfg.PurgeRetentionDays = 30
	}
	if cfg.PurgeInterval <= 0 {
		log.Printf("config: PURGE_INTERVAL_HOURS invalid (must be > 0); using 24h")
		cfg.PurgeInterval = 24 * time.Hour
	}

	return cfg
}

// Validate enforces security-critical configuration. In production it FAILS
// (returns an error the caller turns into a fatal boot error) on insecure
// defaults; in non-production it only warns, so local dev stays frictionless.
//
// Rationale for each rule:
//   - JWT secret: the default/short key is public knowledge → anyone can forge a
//     token for any user (incl. OWNER). Never allow it in production.
//   - Demo users + default password: owner@the.local … with a well-known password
//     are real, working accounts. Must not exist in a production database.
//   - CORS "*": reflecting any origin is unsafe cross-site; warn loudly in prod.
func (c *Config) Validate() error {
	var problems []string

	insecureJWT := c.JWTSecret == DefaultJWTSecret || len(c.JWTSecret) < MinJWTSecretLen
	demoUsersOn := c.SeedOnStart && c.SeedDemoUsers
	demoWithDefaultPw := demoUsersOn && c.DemoPassword == DefaultDemoPassword
	demoWithWeakPw := demoUsersOn && len(c.DemoPassword) < 10
	corsWildcard := false
	for _, o := range c.CORSAllowedOrigins {
		if o == "*" {
			corsWildcard = true
		}
	}

	// Enforce (fatal) for any environment that isn't clearly dev/test, so a deploy
	// with APP_ENV set to "prod"/"staging"/"live" — or forgotten and left at a
	// non-dev value — still gets the hard checks instead of a silent warning.
	if c.requiresSecureConfig() {
		if insecureJWT {
			problems = append(problems, fmt.Sprintf(
				"JWT_SECRET is the default or shorter than %d chars — set a long random secret", MinJWTSecretLen))
		}
		if demoWithDefaultPw {
			problems = append(problems,
				"demo users are enabled with the default SEED_DEMO_PASSWORD — set SEED_DEMO_USERS=false in production, or a strong SEED_DEMO_PASSWORD")
		} else if demoWithWeakPw {
			problems = append(problems,
				"demo users are enabled with a weak SEED_DEMO_PASSWORD (<10 chars) — set SEED_DEMO_USERS=false or a stronger password")
		}
		if corsWildcard {
			// Not fatal (some deployments front the API with a gateway), but loud.
			log.Println("config: WARNING — CORS_ALLOWED_ORIGINS='*' in production reflects any origin; set explicit origins")
		}
		if len(problems) > 0 {
			return fmt.Errorf("insecure production config:\n  - %s", strings.Join(problems, "\n  - "))
		}
		return nil
	}

	// Non-production: warn only, never block.
	if insecureJWT {
		log.Println("config: WARNING — using the default/short JWT_SECRET (dev only; MUST change before production)")
	}
	if demoWithDefaultPw {
		log.Println("config: WARNING — demo users seeded with the default password (dev only; disable or change before production)")
	}
	return nil
}

// DSN builds the PostgreSQL connection string for GORM.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode, c.DBTimeZone,
	)
}

// IsProduction reports whether the app is running in a production environment.
func (c *Config) IsProduction() bool {
	return strings.EqualFold(c.AppEnv, "production")
}

// requiresSecureConfig is true unless AppEnv is an explicit dev/test value. This
// deliberately treats unknown envs as needing the strict security checks, so a
// mislabeled ("prod", "staging", "live") deploy can't slip past on a warning.
func (c *Config) requiresSecureConfig() bool {
	switch strings.ToLower(strings.TrimSpace(c.AppEnv)) {
	case "development", "dev", "test", "testing", "local":
		return false
	default:
		return true
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getEnvAsInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvAsBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
