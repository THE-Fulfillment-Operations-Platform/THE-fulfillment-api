package models

import "time"

// DailyCounter is an atomic per-day sequence allocator. There is exactly one row
// per business calendar day (in the DB_TIMEZONE), holding the highest sequence
// number handed out that day. Order creation upserts this row
// (INSERT ... ON CONFLICT (scope_date) DO UPDATE ... RETURNING seq) so any number
// of concurrent inserts each receive a distinct, monotonically increasing daily
// number without a separate lock round-trip. This backs Order.DailySeq
// ("STT trong ngày").
type DailyCounter struct {
	ScopeDate string    `json:"scope_date" gorm:"primaryKey;size:10"` // YYYY-MM-DD in the business timezone
	Seq       int       `json:"seq" gorm:"not null;default:0"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (DailyCounter) TableName() string { return "daily_counters" }
