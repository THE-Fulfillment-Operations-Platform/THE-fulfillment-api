package models

import (
	"time"

	"gorm.io/gorm"
)

// Base is embedded in every persistent model. It provides an auto-increment
// primary key, created/updated timestamps and soft-delete support.
type Base struct {
	ID        uint           `json:"id" gorm:"primaryKey"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-" gorm:"index"`
}

// JSONMap is a convenience type for storing flexible metadata as JSONB.
type JSONMap map[string]interface{}
