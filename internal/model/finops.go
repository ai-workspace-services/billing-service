package model

import (
	"time"

	"github.com/google/uuid"
)

// CloudVendorCost represents an aggregated cost record from a cloud provider (AWS/GCP/Azure/etc).
// It maps directly to the public.cloud_vendor_costs table.
type CloudVendorCost struct {
	ID             uuid.UUID `db:"id"`
	Provider       string    `db:"provider"`
	AccountID      string    `db:"account_id"`
	ServiceName    string    `db:"service_name"`
	Region         string    `db:"region"`
	UsageStartTime time.Time `db:"usage_start_time"`
	UsageEndTime   time.Time `db:"usage_end_time"`
	CostAmount     float64   `db:"cost_amount"`
	Currency       string    `db:"currency"`
	UsageQuantity  *float64  `db:"usage_quantity"`
	UsageUnit      *string   `db:"usage_unit"`
	CreatedAt      time.Time `db:"created_at"`
}
