package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"billing-service/internal/model"
)

type recordedQuery struct {
	query string
	args  []driver.Value
}

type stubDBFixture struct {
	execs   []recordedQuery
	queries []recordedQuery

	queryHandler func(query string, args []driver.Value) (columns []string, rows [][]driver.Value, err error)
}

type stubConnector struct{ f *stubDBFixture }

func (c stubConnector) Connect(context.Context) (driver.Conn, error) {
	return stubConn{c.f}, nil
}
func (c stubConnector) Driver() driver.Driver { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use stubConnector")
}

type stubConn struct{ f *stubDBFixture }

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not implemented") }
func (stubConn) Close() error                       { return nil }
func (stubConn) Begin() (driver.Tx, error)          { return nil, errors.New("not implemented") }

func (c stubConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.f.execs = append(c.f.execs, recordedQuery{query: query, args: vals})
	return driver.RowsAffected(1), nil
}

func (c stubConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.f.queries = append(c.f.queries, recordedQuery{query: query, args: vals})
	if c.f.queryHandler == nil {
		return &stubRows{}, nil
	}
	cols, rows, err := c.f.queryHandler(query, vals)
	if err != nil {
		return nil, err
	}
	return &stubRows{columns: cols, rows: rows}, nil
}

type stubRows struct {
	columns []string
	rows    [][]driver.Value
}

func (r *stubRows) Columns() []string { return r.columns }
func (*stubRows) Close() error        { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

func openStubPostgres(t *testing.T, fixture *stubDBFixture) *Postgres {
	t.Helper()
	db := sql.OpenDB(stubConnector{f: fixture})
	t.Cleanup(func() { _ = db.Close() })
	return NewPostgres(db)
}

func TestPostgresUpsertMinuteBucketPersistsRoutingPool(t *testing.T) {
	fixture := &stubDBFixture{
		queryHandler: func(query string, _ []driver.Value) ([]string, [][]driver.Value, error) {
			if strings.Contains(query, "FROM traffic_minute_buckets") {
				return []string{"marker"}, nil, nil
			}
			return nil, nil, nil
		},
	}
	pg := openStubPostgres(t, fixture)

	existed, err := pg.UpsertMinuteBucket(context.Background(), model.MinuteBucket{
		BucketStart:    time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC),
		NodeID:         "prod:jp-xconnect-pro-01.svc.plus",
		AccountUUID:    "11111111-1111-1111-1111-111111111111",
		RoutingPool:    "Pro-EC2-Pool",
		UplinkBytes:    100,
		DownlinkBytes:  200,
		TotalBytes:     300,
		Multiplier:     1.0,
		RatingStatus:   "rated",
		SourceRevision: "v1",
	})
	if err != nil {
		t.Fatalf("UpsertMinuteBucket: %v", err)
	}
	if existed {
		t.Fatalf("expected existed=false")
	}
	if len(fixture.execs) != 1 {
		t.Fatalf("expected 1 exec, got %d", len(fixture.execs))
	}
	exec := fixture.execs[0]
	if !strings.Contains(exec.query, "routing_pool") {
		t.Fatalf("expected routing_pool in insert query: %s", exec.query)
	}
	if got := exec.args[5]; got != "Pro-EC2-Pool" {
		t.Fatalf("expected routing_pool arg Pro-EC2-Pool, got %v", got)
	}
}

func TestPostgresQuotaAndProfileDualBucketFields(t *testing.T) {
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	fixture := &stubDBFixture{
		queryHandler: func(query string, _ []driver.Value) ([]string, [][]driver.Value, error) {
			switch {
			case strings.Contains(query, "FROM account_quota_states") && strings.Contains(query, "WHERE account_uuid = $1"):
				return []string{
					"account_uuid", "remaining_included_quota", "remaining_high_speed_quota",
					"current_balance", "arrears", "arrears_since", "throttle_state",
					"suspend_state", "proxy_access_state", "last_rated_bucket_at", "effective_at", "period_start", "period_end",
				}, [][]driver.Value{{
					"11111111-1111-1111-1111-111111111111", int64(5000), int64(3000),
					25.5, false, nil, "normal", "active", "active", now, now, now, now.Add(24 * time.Hour),
				}}, nil
			case strings.Contains(query, "FROM account_quota_states") && strings.Contains(query, "arrears = true"):
				return []string{
					"account_uuid", "remaining_included_quota", "remaining_high_speed_quota",
					"current_balance", "arrears", "arrears_since", "throttle_state",
					"suspend_state", "proxy_access_state", "last_rated_bucket_at", "effective_at",
				}, [][]driver.Value{{
					"11111111-1111-1111-1111-111111111111", int64(100), int64(200),
					-5.0, true, now, "throttled", "active", "active", now, now,
				}}, nil
			case strings.Contains(query, "FROM account_billing_profiles"):
				return []string{
					"account_uuid", "package_name", "included_quota_bytes", "high_speed_quota_bytes",
					"base_price_per_byte", "region_multiplier", "line_multiplier",
					"peak_multiplier", "offpeak_multiplier", "pricing_rule_version",
				}, [][]driver.Value{{
					"11111111-1111-1111-1111-111111111111", "pro", int64(53687091200), int64(32212254720),
					0.0, 1.0, 1.0, 1.0, 1.0, "pro-v2",
				}}, nil
			default:
				return nil, nil, nil
			}
		},
	}
	pg := openStubPostgres(t, fixture)
	ctx := context.Background()

	quota, err := pg.GetQuotaState(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("GetQuotaState: %v", err)
	}
	if quota == nil || quota.RemainingIncludedQuota != 5000 || quota.RemainingHighSpeedQuota != 3000 {
		t.Fatalf("unexpected quota: %+v", quota)
	}

	if err := pg.UpsertQuotaState(ctx, model.QuotaState{
		AccountUUID:             "11111111-1111-1111-1111-111111111111",
		RemainingIncludedQuota:  4000,
		RemainingHighSpeedQuota: 2500,
		CurrentBalance:          25.5,
		ThrottleState:           "normal",
		SuspendState:            "active",
		EffectiveAt:             now,
	}); err != nil {
		t.Fatalf("UpsertQuotaState: %v", err)
	}
	lastExec := fixture.execs[len(fixture.execs)-1]
	if !strings.Contains(lastExec.query, "remaining_high_speed_quota") || !strings.Contains(lastExec.query, "proxy_access_state") {
		t.Fatalf("expected remaining_high_speed_quota and proxy_access_state in UpsertQuotaState: %s", lastExec.query)
	}
	if lastExec.args[2] != int64(2500) {
		t.Fatalf("expected remaining_high_speed_quota arg 2500, got %v", lastExec.args[2])
	}

	arrears, err := pg.ListArrearsAccounts(ctx)
	if err != nil {
		t.Fatalf("ListArrearsAccounts: %v", err)
	}
	if len(arrears) != 1 || arrears[0].RemainingHighSpeedQuota != 200 {
		t.Fatalf("expected ListArrearsAccounts to preserve RemainingHighSpeedQuota=200, got %+v", arrears)
	}

	profile, err := pg.GetBillingProfile(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("GetBillingProfile: %v", err)
	}
	if profile == nil || profile.IncludedQuotaBytes != 53687091200 || profile.HighSpeedQuotaBytes != 32212254720 {
		t.Fatalf("unexpected profile: %+v", profile)
	}
}

func TestPostgresGetNodePoolMetadata(t *testing.T) {
	fixture := &stubDBFixture{
		queryHandler: func(_ string, args []driver.Value) ([]string, [][]driver.Value, error) {
			switch args[0] {
			case "jp-xconnect-pro-01.svc.plus":
				return []string{"found", "groups", "pricing_group"}, [][]driver.Value{
					{true, []byte(`["resource:pro"]`), "pro"},
				}, nil
			default:
				return []string{"found", "groups", "pricing_group"}, [][]driver.Value{
					{false, []byte(`[]`), ""},
				}, nil
			}
		},
	}
	pg := openStubPostgres(t, fixture)

	meta, err := pg.GetNodePoolMetadata(context.Background(), "jp-xconnect-pro-01.svc.plus")
	if err != nil {
		t.Fatalf("GetNodePoolMetadata found: %v", err)
	}
	if meta == nil || len(meta.Groups) != 1 || meta.Groups[0] != "resource:pro" || meta.PricingGroup != "pro" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}

	missing, err := pg.GetNodePoolMetadata(context.Background(), "unknown.svc.plus")
	if err != nil {
		t.Fatalf("GetNodePoolMetadata missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("expected nil metadata for unknown node, got %+v", missing)
	}
}
