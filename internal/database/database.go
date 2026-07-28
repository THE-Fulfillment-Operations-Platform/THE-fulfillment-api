// Package database wires up the GORM connection to PostgreSQL and runs the
// auto-migration for all models.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

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

// KeepWarm re-proves the pool's connections alive on an interval, returning
// once ctx is cancelled. warmPool below covers start-up; this covers everything
// after it.
//
// Go will happily hold an idle connection for DB_CONN_MAX_IDLE_MINUTES, but the
// other end may not: hosted poolers and NAT gateways drop idle TCP sessions
// without telling the client, so the socket looks fine in the pool and only
// fails when a real request picks it up. That request then pays a full
// handshake — TCP + TLS + pooler auth, measured at 1-2 SECONDS to Supabase's
// ap-southeast-1 pooler — and pays it once per statement it runs, which is how a
// five-statement bulk endpoint answers in six seconds while every individual
// query is fast. Pinging on a timer moves that cost off the user's request and
// into the background.
//
// Keep `every` comfortably below both DB_CONN_MAX_IDLE_MINUTES and the
// provider's own idle timeout.
func KeepWarm(ctx context.Context, db *gorm.DB, conns int, every time.Duration) {
	sqlDB, err := db.DB()
	if err != nil {
		log.Printf("database: keepalive disabled: %v", err)
		return
	}
	log.Printf("database: keepalive on (%d connections, every %s)", conns, every)

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if got := touchPool(sqlDB, conns, 10*time.Second); got < conns {
				log.Printf("database: keepalive reached %d/%d connections", got, conns)
			}
		}
	}
}

// warmPool opens the idle connections up front so the first users don't pay for
// them. The pool only ever creates connections lazily, on a request that is
// already waiting, so without this the handshake cost lands on whoever clicks
// first — and again after every restart.
func warmPool(sqlDB *sql.DB, n int) {
	if got := touchPool(sqlDB, n, 10*time.Second); got < n {
		log.Printf("database: warmed %d/%d connections", got, n)
	}
}

// touchPool proves up to n pooled connections usable, opening any the pool is
// missing, and reports how many it ended up with.
//
// The connections are held SIMULTANEOUSLY on purpose: that is what makes
// database/sql hand out n distinct sockets rather than serving every request
// from the same one (which is also why sql.DB.Ping alone cannot warm a pool).
// Closing a *sql.Conn returns it to the idle list, not to the network. The ping
// is the part that matters for a connection that has been sitting: Conn() hands
// back a pooled socket without checking it, and only a round trip reveals one
// the far end has quietly closed — discarding it here means the pool replaces it
// on our time rather than mid-request.
//
// Best-effort and time-boxed throughout: a slow or unreachable database must
// never block start-up or wedge the keepalive goroutine.
func touchPool(sqlDB *sql.DB, n int, timeout time.Duration) int {
	if n < 1 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conns := make([]*sql.Conn, 0, n)
	// Bounded attempts, not a fixed n iterations: a dead connection costs an
	// attempt, and we still want to end up with n live ones when we can.
	for attempts := 0; len(conns) < n && attempts < 2*n; attempts++ {
		c, err := sqlDB.Conn(ctx)
		if err != nil {
			break // timed out or pool exhausted — keep what we got
		}
		if err := c.PingContext(ctx); err != nil {
			_ = c.Close() // stale: database/sql drops it instead of pooling it
			continue
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		_ = c.Close() // back to the idle pool, not to the network
	}
	return len(conns)
}
