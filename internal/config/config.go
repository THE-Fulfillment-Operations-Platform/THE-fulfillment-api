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

	// Connection pool. Defaults are sized for a small VPS (2 vCPU / 4 GB RAM)
	// sharing the box with PostgreSQL: enough concurrency for an internal ops
	// tool, small enough that Postgres never sees a connection storm.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	DBConnMaxIdleTime time.Duration
	// How often the pool's connections are pinged to keep them (and their cached
	// prepared statements) alive. 0 turns the keepalive off.
	DBKeepAliveInterval time.Duration
	// Server-side cap on how long any single SQL statement may run. A runaway
	// query gets cancelled by Postgres instead of pinning a pooled connection
	// (and, behind a shared pooler, one of the precious client slots) forever.
	// 0 disables the cap.
	DBStatementTimeout time.Duration

	// Largest accepted request body. Uploads (Excel imports) are buffered in
	// memory while parsing, so this is also a memory-safety cap.
	MaxBodyBytes int64

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
	SeedOnStart   bool
	SeedDemoUsers bool
	DemoPassword  string

	// Ops / danger zone
	AllowDataReset bool

	// Maintenance: periodically hard-delete rows soft-deleted longer ago than the
	// retention window, so GORM soft-deletes don't accumulate without bound.
	PurgeEnabled       bool
	PurgeRetentionDays int
	PurgeInterval      time.Duration

	// Tracking provider (24hTrack). Empty credentials disable the integration
	// entirely: tracking then stays a manual field, exactly as before.
	Track24hEnabled  bool
	Track24hBaseURL  string
	Track24hEmail    string
	Track24hPassword string
	// Track24hTag prefixes the description we write on the provider so our
	// shipments are distinguishable from anything else in the same account, and so
	// a store order id can be searched for without matching unrelated text.
	Track24hTag string
	// Track24hSyncInterval is how often the background sync runs; BatchSize caps
	// how many parcels one run refreshes (the provider rate-limits per minute).
	Track24hSyncInterval time.Duration
	Track24hBatchSize    int
	// Track24hResolve turns on the reverse lookup: for an order that has no
	// tracking number yet, ask the provider whether a parcel was registered under
	// this store order id. Only useful once shipments are tagged that way.
	Track24hResolve bool
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

		DBMaxOpenConns: getEnvAsInt("DB_MAX_OPEN_CONNS", 15),
		// Idle == open on purpose. Opening a connection to a hosted Postgres costs
		// a TCP+TLS+auth handshake — measured at ~1s to a Supabase pooler — while
		// reusing a warm one costs a single round-trip. Keeping fewer idle than open
		// means every burst of parallel requests (the app fires 6-8 per screen)
		// closes the surplus straight after use, so the next burst pays the handshake
		// all over again. Warm connections cost the database almost nothing here.
		DBMaxIdleConns:    getEnvAsInt("DB_MAX_IDLE_CONNS", getEnvAsInt("DB_MAX_OPEN_CONNS", 15)),
		DBConnMaxLifetime: time.Duration(getEnvAsInt("DB_CONN_MAX_LIFETIME_MINUTES", 55)) * time.Minute,
		// Long, for the same reason: an ops tool goes quiet for minutes at a time and
		// must not pay a full handshake on the next click.
		DBConnMaxIdleTime: time.Duration(getEnvAsInt("DB_CONN_MAX_IDLE_MINUTES", 30)) * time.Minute,
		// Holding a connection open is not the same as it still working: hosted
		// poolers and NAT gateways drop idle TCP sessions silently, so a pool told to
		// keep connections for 30 minutes still hands out dead ones. 60s is well
		// inside the shortest idle timeout we have to survive.
		DBKeepAliveInterval: time.Duration(getEnvAsInt("DB_KEEPALIVE_SECONDS", 60)) * time.Second,
		// 60s fits every normal query with a wide margin (list pages answer in
		// milliseconds) while still cutting genuinely stuck statements loose. Raise
		// it temporarily for heavy one-off migrations.
		DBStatementTimeout: time.Duration(getEnvAsInt("DB_STATEMENT_TIMEOUT_SECONDS", 60)) * time.Second,

		MaxBodyBytes: int64(getEnvAsInt("MAX_BODY_MB", 32)) << 20,

		JWTSecret:    getEnv("JWT_SECRET", "change-me-in-production"),
		JWTExpiresIn: time.Duration(getEnvAsInt("JWT_EXPIRES_HOURS", 72)) * time.Hour,

		CORSAllowedOrigins: splitAndTrim(getEnv("CORS_ALLOWED_ORIGINS", "*")),

		TrustedProxies: splitAndTrim(getEnv("TRUSTED_PROXIES", "")),

		SeedOnStart:   getEnvAsBool("SEED_ON_START", true),
		SeedDemoUsers: getEnvAsBool("SEED_DEMO_USERS", true),
		DemoPassword:  getEnv("SEED_DEMO_PASSWORD", DefaultDemoPassword),

		AllowDataReset: getEnvAsBool("ALLOW_DATA_RESET", false),

		PurgeEnabled:       getEnvAsBool("PURGE_ENABLED", true),
		PurgeRetentionDays: getEnvAsInt("PURGE_RETENTION_DAYS", 30),
		PurgeInterval:      time.Duration(getEnvAsInt("PURGE_INTERVAL_HOURS", 24)) * time.Hour,

		Track24hEnabled:  getEnvAsBool("TRACK24H_ENABLED", false),
		Track24hBaseURL:  getEnv("TRACK24H_BASE_URL", "https://api.24htrack.com"),
		Track24hEmail:    getEnv("TRACK24H_EMAIL", ""),
		Track24hPassword: getEnv("TRACK24H_PASSWORD", ""),
		Track24hTag:      getEnv("TRACK24H_TAG", "FFM"),
		// 20 minutes is a deliberate floor on chattiness: carriers scan a parcel a
		// handful of times a day, so polling faster buys nothing and spends the
		// provider's per-minute budget that a large batch needs.
		Track24hSyncInterval: time.Duration(getEnvAsInt("TRACK24H_SYNC_INTERVAL_MINUTES", 20)) * time.Minute,
		// Each parcel costs up to 2 provider calls (detail + timeline). 120 parcels
		// therefore fits one run comfortably inside the 200 requests/minute limit
		// once the client's own inter-call floor is applied.
		Track24hBatchSize: getEnvAsInt("TRACK24H_BATCH_SIZE", 120),
		Track24hResolve:   getEnvAsBool("TRACK24H_RESOLVE", true),
	}

	// Credentials are what actually make the integration work; a TRACK24H_ENABLED
	// with no login would just log a failure every cycle.
	if cfg.Track24hEnabled && (cfg.Track24hEmail == "" || cfg.Track24hPassword == "") {
		log.Println("config: TRACK24H_ENABLED=true but TRACK24H_EMAIL/TRACK24H_PASSWORD are empty; tracking sync stays off")
		cfg.Track24hEnabled = false
	}
	if cfg.Track24hSyncInterval < time.Minute {
		log.Printf("config: TRACK24H_SYNC_INTERVAL_MINUTES too small; using 20m")
		cfg.Track24hSyncInterval = 20 * time.Minute
	}
	if cfg.Track24hBatchSize <= 0 {
		cfg.Track24hBatchSize = 120
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

	// Guard the pool: zero/negative values would mean "unlimited open conns" or a
	// dead pool, either of which can knock over a small Postgres.
	if cfg.DBMaxOpenConns <= 0 {
		log.Printf("config: DB_MAX_OPEN_CONNS=%d invalid; using 15", cfg.DBMaxOpenConns)
		cfg.DBMaxOpenConns = 15
	}
	if cfg.DBMaxIdleConns <= 0 || cfg.DBMaxIdleConns > cfg.DBMaxOpenConns {
		cfg.DBMaxIdleConns = cfg.DBMaxOpenConns / 3
		if cfg.DBMaxIdleConns < 1 {
			cfg.DBMaxIdleConns = 1
		}
	}
	if cfg.DBConnMaxLifetime <= 0 {
		cfg.DBConnMaxLifetime = 30 * time.Minute
	}
	if cfg.DBConnMaxIdleTime <= 0 {
		cfg.DBConnMaxIdleTime = 5 * time.Minute
	}
	if cfg.DBStatementTimeout < 0 {
		cfg.DBStatementTimeout = 0 // negative makes no sense; treat as disabled
	}

	// Guard the body cap: a non-positive value would reject every request (or,
	// unbounded, allow a memory-exhausting upload), so fall back to 32 MB.
	if cfg.MaxBodyBytes <= 0 {
		log.Printf("config: MAX_BODY_MB invalid (must be > 0); using 32")
		cfg.MaxBodyBytes = 32 << 20
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
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode, c.DBTimeZone,
	)
	// Sent as a startup parameter so EVERY pooled connection gets the cap —
	// a post-connect `SET` would only reach whichever connection ran it.
	if c.DBStatementTimeout > 0 {
		dsn += fmt.Sprintf(" options='-c statement_timeout=%d'", c.DBStatementTimeout.Milliseconds())
	}
	return dsn
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
