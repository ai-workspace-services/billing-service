package model

import "time"

type Sample struct {
	UUID               string `json:"uuid"`
	Email              string `json:"email"`
	InboundTag         string `json:"inbound_tag"`
	UplinkBytesTotal   int64  `json:"uplink_bytes_total"`
	DownlinkBytesTotal int64  `json:"downlink_bytes_total"`
}

type Snapshot struct {
	CollectedAt time.Time `json:"collected_at"`
	NodeID      string    `json:"node_id"`
	Env         string    `json:"env"`
	Samples     []Sample  `json:"samples"`
}

type SnapshotWindowPage struct {
	NodeID     string     `json:"node_id"`
	Env        string     `json:"env"`
	Snapshots  []Snapshot `json:"snapshots"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type Checkpoint struct {
	NodeID            string
	AccountUUID       string
	LastUplinkTotal   int64
	LastDownlinkTotal int64
	LastSeenAt        time.Time
	XrayRevision      string
	ResetEpoch        int64
}

type MinuteBucket struct {
	BucketStart    time.Time
	NodeID         string
	AccountUUID    string
	Region         string
	LineCode       string
	UplinkBytes    int64
	DownlinkBytes  int64
	TotalBytes     int64
	Multiplier     float64
	RatingStatus   string
	SourceRevision string
}

type LedgerEntry struct {
	ID                 string
	AccountUUID        string
	BucketStart        time.Time
	BucketEnd          time.Time
	EntryType          string
	RatedBytes         int64
	AmountDelta        float64
	BalanceAfter       float64
	PricingRuleVersion string
}

type QuotaState struct {
	AccountUUID            string
	RemainingIncludedQuota int64
	CurrentBalance         float64
	Arrears                bool
	// ArrearsSince marks when Arrears last flipped false->true. It is cleared
	// (nil) whenever Arrears clears, so it always reflects the length of the
	// current arrears episode rather than the account's first-ever failure.
	ArrearsSince      *time.Time
	ThrottleState     string
	SuspendState      string
	LastRatedBucketAt *time.Time
	EffectiveAt       time.Time
}

type BillingProfile struct {
	AccountUUID        string
	PackageName        string
	IncludedQuotaBytes int64
	BasePricePerByte   float64
	RegionMultiplier   float64
	LineMultiplier     float64
	PeakMultiplier     float64
	OffPeakMultiplier  float64
	PricingRuleVersion string
}

type SourceSyncState struct {
	SourceID           string
	LastCompletedUntil *time.Time
	LastAttemptedAt    *time.Time
	LastSucceededAt    *time.Time
	LastError          string
}

type SourceStatus struct {
	SourceID           string     `json:"source_id"`
	LastCompletedUntil *time.Time `json:"last_completed_until,omitempty"`
	LastAttemptedAt    *time.Time `json:"last_attempted_at,omitempty"`
	LastSucceededAt    *time.Time `json:"last_succeeded_at,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
}

type JobResult struct {
	Job              string         `json:"job"`
	StartedAt        time.Time      `json:"started_at"`
	FinishedAt       time.Time      `json:"finished_at"`
	ProcessedSamples int            `json:"processed_samples"`
	WrittenMinutes   int            `json:"written_minutes"`
	ReplayedMinutes  int            `json:"replayed_minutes"`
	Status           string         `json:"status"`
	Error            string         `json:"error,omitempty"`
	SourceStatuses   []SourceStatus `json:"source_statuses,omitempty"`
}

type PingInfo struct {
	Image   string `json:"image"`
	Tag     string `json:"tag,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Version string `json:"version,omitempty"`
}
