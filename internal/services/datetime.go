package services

import (
	"os"
	"sync"
	"time"
)

// appLocation is the business timezone used for "STT trong ngày" and OrderDate.
// It is DB_TIMEZONE (matching the DB session timezone), defaulting to
// Asia/Ho_Chi_Minh, and falls back to UTC if the zone can't be loaded. Loaded
// once. cmd/server imports time/tzdata so the zone is always available even on a
// minimal container image without the system zoneinfo database.
var (
	appLocOnce sync.Once
	appLoc     *time.Location
)

// AppLocation returns the configured business timezone.
func AppLocation() *time.Location {
	appLocOnce.Do(func() {
		name := os.Getenv("DB_TIMEZONE")
		if name == "" {
			name = "Asia/Ho_Chi_Minh"
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			loc = time.UTC
		}
		appLoc = loc
	})
	return appLoc
}

// AppDateString formats t as the business calendar day (YYYY-MM-DD) in the
// business timezone. This is the value stored in Order.OrderDate.
func AppDateString(t time.Time) string {
	return t.In(AppLocation()).Format("2006-01-02")
}
