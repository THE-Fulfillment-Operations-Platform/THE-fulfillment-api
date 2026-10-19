package models

import "time"

// DataMigration records a one-time data change that already ran. Most backfills
// here are idempotent by looking at the data itself; this is for the ones that
// cannot be — where running again would undo a choice a person made afterwards.
type DataMigration struct {
	Name      string    `json:"name" gorm:"primaryKey;size:100"`
	AppliedAt time.Time `json:"applied_at" gorm:"not null"`
}

func (DataMigration) TableName() string { return "data_migrations" }

// AddImpliedTicks adds, for every key of implied present in list, its value
// (e.g. qc_results.view → qc_results.manage). Returns the normalised list and
// whether anything was added.
func AddImpliedTicks(list PermList, implied map[string]string) (PermList, bool) {
	have := make(map[string]bool, len(list))
	for _, p := range list {
		have[p] = true
	}
	next := append([]string{}, list...)
	added := false
	for from, to := range implied {
		if have[from] && !have[to] {
			next = append(next, to)
			added = true
		}
	}
	if !added {
		return list, false
	}
	return PermList(NormalizePerms(next)), true
}
