package value_objects

import (
	"time"

	"github.com/shopspring/decimal"
)

// ModelCallRecord records one attempted model request, including failed requests.
type ModelCallRecord struct {
	Pending         bool
	ReservedCostUSD decimal.Decimal
	Round           int
	Model           string
	Provider        string
	StartedAt       time.Time
	Duration        time.Duration
	Usage           Usage
	FinishReason    string
	Failed          bool
}
