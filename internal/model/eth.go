package model
import(
	"time"
)
type ScanProgress struct {
	Chain           Chain     `gorm:"primaryKey;type:varchar(32)" json:"chain"`
	LastScannedBlock uint64    `gorm:"not null" json:"last_scanned_block"`
	UpdatedAt       time.Time `json:"updated_at"`
}