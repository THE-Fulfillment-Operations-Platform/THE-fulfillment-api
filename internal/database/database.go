// Package database wires up the GORM connection to PostgreSQL and runs the
// auto-migration for all models.
package database

import (
	"fmt"
	"log"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"the-fulfillment/backend/internal/config"
)

// Connect opens a pooled GORM connection to PostgreSQL using the provided config.
func Connect(cfg *config.Config) (*gorm.DB, error) {
	logLevel := gormlogger.Warn
	if !cfg.IsProduction() {
		logLevel = gormlogger.Info
	}

	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger:                                   gormlogger.Default.LogMode(logLevel),
		DisableForeignKeyConstraintWhenMigrating: false,
		// Cache prepared statements per connection: repeated queries (list pages,
		// lookups by id/code) skip re-parsing/planning on every call.
		PrepareStmt: true,
	})
	if err != nil {
		return nil, fmt.Errorf("database: open: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("database: underlying sql.DB: %w", err)
	}
	// Pool sizing comes from config (defaults tuned for a 2 vCPU / 4 GB VPS that
	// also hosts Postgres). MaxIdleTime releases idle conns back to Postgres so a
	// nightly-quiet ops tool doesn't pin memory; MaxLifetime recycles conns so a
	// failover / pgbouncer restart can't leave half-dead sockets in the pool.
	sqlDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.DBConnMaxIdleTime)

	log.Printf("database: connected to %s:%s/%s", cfg.DBHost, cfg.DBPort, cfg.DBName)
	return db, nil
}
