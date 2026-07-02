package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// JSONB is a Postgres jsonb column backed by raw bytes. It is used for flexible
// payloads (import rows, audit metadata) without pulling in an extra dependency.
type JSONB []byte

// Value implements driver.Valuer.
func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner.
func (j *JSONB) Scan(value interface{}) error {
	if value == nil {
		*j = nil
		return nil
	}
	switch v := value.(type) {
	case []byte:
		*j = append((*j)[0:0], v...)
		return nil
	case string:
		*j = []byte(v)
		return nil
	default:
		return errors.New("JSONB: unsupported scan type")
	}
}

// MarshalJSON makes JSONB transparent in API responses.
func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON stores the raw JSON bytes.
func (j *JSONB) UnmarshalJSON(data []byte) error {
	if j == nil {
		return errors.New("JSONB: UnmarshalJSON on nil pointer")
	}
	*j = append((*j)[0:0], data...)
	return nil
}

// GormDataType tells GORM the column type.
func (JSONB) GormDataType() string { return "jsonb" }

// ToJSONB marshals any value into a JSONB column value.
func ToJSONB(v interface{}) (JSONB, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ToJSONB: %w", err)
	}
	return JSONB(b), nil
}
