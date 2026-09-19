package model

import (
	"time"
)

type ScanProgress struct {
	Chain            Chain     `gorm:"primaryKey;type:varchar(32)" json:"chain"`
	Address          string    `gorm:"primaryKey;type:varchar(128);default:'';" json:"address"`
	LastScannedBlock uint64    `gorm:"default:0" json:"last_scanned_block"`
	LastSignature    string    `gorm:"type:varchar(128);default:''" json:"last_signature"`
	UpdatedAt        time.Time `json:"updated_at"`
}
