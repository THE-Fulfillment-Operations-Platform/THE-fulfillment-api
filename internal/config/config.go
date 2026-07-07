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

	// Seed
	SeedOnStart    bool
	SeedDemoOrders bool
	DemoPassword   string

	// Ops / danger zone
	AllowDataReset bool
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

		SeedOnStart:    getEnvAsBool("SEED_ON_START", true),
		SeedDemoOrders: getEnvAsBool("SEED_DEMO_ORDERS", false),
		DemoPassword:   getEnv("SEED_DEMO_PASSWORD", "Password123!"),

		AllowDataReset: getEnvAsBool("ALLOW_DATA_RESET", false),
	}

	return cfg
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
