package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"
)

// The driver supplies catalog query results without a server or write access.
// Real PostgreSQL catalog/startup checks remain in database_runtime_test.go.
type checkpointFixture struct {
	version, migrationRows, relations int64
	dirty, badColumns, committed      bool
	badReceipt                        bool
}

type checkpointConnector struct{ f *checkpointFixture }

func (c checkpointConnector) Connect(context.Context) (driver.Conn, error) {
	return checkpointConn{c.f}, nil
}
func (c checkpointConnector) Driver() driver.Driver { return checkpointDriver{} }

type checkpointDriver struct{}

func (checkpointDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use isolated connector")
}

type checkpointConn struct{ f *checkpointFixture }

func (checkpointConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("statements forbidden")
}
func (checkpointConn) Close() error { return nil }
func (checkpointConn) Begin() (driver.Tx, error) {
	return nil, errors.New("explicit readonly transaction required")
}
func (c checkpointConn) BeginTx(_ context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if !opts.ReadOnly || opts.Isolation != driver.IsolationLevel(sql.LevelRepeatableRead) {
		return nil, errors.New("verification must use a readonly repeatable-read transaction")
	}
	return checkpointTx{c.f}, nil
}

type checkpointTx struct{ f *checkpointFixture }

func (t checkpointTx) Commit() error { t.f.committed = true; return nil }
func (checkpointTx) Rollback() error { return nil }

type checkpointRows struct {
	columns []string
	values  [][]driver.Value
}

func (r *checkpointRows) Columns() []string { return r.columns }
func (*checkpointRows) Close() error        { return nil }
func (r *checkpointRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}
func (c checkpointConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	rows := func(columns []string, values ...[]driver.Value) (driver.Rows, error) {
		return &checkpointRows{columns, values}, nil
	}
	switch {
	case strings.Contains(q, "FROM public.accounts_controlled_migration_history"):
		from, hash := int64(2026101703), "b9fee298512e7d888fe619917c8238120f0e227f4ea348e59397d074c41f878d"
		if args[0].Value == int64(2026101902) {
			from, hash = 2026101901, "53f9aaf7c56a14fd7c132c6649b79582cd2063f8903b94efb4e23cbf4895437c"
		}
		if c.f.badReceipt {
			hash = "wrong"
		}
		return rows([]string{"from_version", "sql_sha256"}, []driver.Value{from, hash})
	case strings.HasPrefix(q, "SELECT current_database()"):
		return rows([]string{"database", "pg_version"}, []driver.Value{"account", int64(170000)})
	case strings.Contains(q, "FROM public.schema_migrations"):
		return rows([]string{"count", "version", "dirty"}, []driver.Value{c.f.migrationRows, c.f.version, c.f.dirty})
	case strings.HasPrefix(q, "SELECT count(*) FROM pg_class"):
		return rows([]string{"count"}, []driver.Value{c.f.relations})
	case strings.HasPrefix(q, "SELECT attname,"):
		definitions := [][]driver.Value{
			{"id", "uuid:false"}, {"provider", "character varying(50):false"},
			{"account_id", "character varying(100):false"}, {"service_name", "character varying(100):false"},
			{"region", "character varying(100):false"}, {"usage_start_time", "timestamp with time zone:false"},
			{"usage_end_time", "timestamp with time zone:false"}, {"cost_amount", "double precision:false"},
			{"currency", "character varying(10):false"}, {"usage_quantity", "double precision:true"},
			{"usage_unit", "character varying(50):true"}, {"created_at", "timestamp with time zone:false"},
		}
		if c.f.badColumns {
			definitions[0][1] = "text:false"
		}
		return rows([]string{"column", "definition"}, definitions...)
	case strings.Contains(q, "c.contype='p'"):
		return rows([]string{"primary_key"}, []driver.Value{"id"})
	case strings.Contains(q, "c.contype='u'"):
		return rows([]string{"unique_key"}, []driver.Value{"provider,account_id,service_name,region,usage_start_time"})
	case strings.HasPrefix(q, "SELECT has_table_privilege"):
		return rows([]string{"visible"}, []driver.Value{true})
	default:
		return nil, errors.New("unexpected query or write rejected")
	}
}

func TestNativeCheckpointCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture checkpointFixture
		want    uint
	}{
		{"legacy", checkpointFixture{version: 2026100701, migrationRows: 1, relations: 53}, 2026100701},
		{"prod_v2", checkpointFixture{version: 2026101502, migrationRows: 1, relations: 64}, 2026101502},
		{"prod_v2_email", checkpointFixture{version: 2026101703, migrationRows: 1, relations: 73}, 2026101703},
		{"catalog_first", checkpointFixture{version: 2026101901, migrationRows: 1, relations: 73}, 2026101901},
		{"catalog_latest", checkpointFixture{version: 2026101902, migrationRows: 1, relations: 73}, 2026101902},
		{"v4_native", checkpointFixture{version: 2026102002, migrationRows: 1, relations: 75}, 2026102002},
		{"v4_dirty", checkpointFixture{version: 2026102002, migrationRows: 1, relations: 75, dirty: true}, 0},
		{"v4_wrong_scope", checkpointFixture{version: 2026102002, migrationRows: 1, relations: 73}, 0},
		{"v4_column_drift", checkpointFixture{version: 2026102002, migrationRows: 1, relations: 75, badColumns: true}, 0},
		{"v4_unreviewed_intermediate", checkpointFixture{version: 2026102001, migrationRows: 1, relations: 74}, 0},
		{"v4_unreviewed_next", checkpointFixture{version: 2026102003, migrationRows: 1, relations: 75}, 0},
		{"catalog_bad_receipt", checkpointFixture{version: 2026101902, migrationRows: 1, relations: 73, badReceipt: true}, 0},
		{"catalog_future", checkpointFixture{version: 2026101903, migrationRows: 1, relations: 73}, 0},
		{"email_dirty", checkpointFixture{version: 2026101703, migrationRows: 1, relations: 73, dirty: true}, 0},
		{"email_wrong_scope", checkpointFixture{version: 2026101703, migrationRows: 1, relations: 64}, 0},
		{"email_column_drift", checkpointFixture{version: 2026101703, migrationRows: 1, relations: 73, badColumns: true}, 0},
		{"unreviewed_email_next", checkpointFixture{version: 2026101704, migrationRows: 1, relations: 73}, 0},
		{"dirty_v2", checkpointFixture{version: 2026101502, migrationRows: 1, relations: 64, dirty: true}, 0},
		{"unknown", checkpointFixture{version: 2026101503, migrationRows: 1, relations: 64}, 0},
		{"missing_checkpoint", checkpointFixture{relations: 64}, 0},
		{"multiple_checkpoints", checkpointFixture{version: 2026101502, migrationRows: 2, relations: 64}, 0},
		{"legacy_wrong_scope", checkpointFixture{version: 2026100701, migrationRows: 1, relations: 64}, 0},
		{"v2_wrong_scope", checkpointFixture{version: 2026101502, migrationRows: 1, relations: 53}, 0},
		{"v2_owned_column_drift", checkpointFixture{version: 2026101502, migrationRows: 1, relations: 64, badColumns: true}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture
			db := sql.OpenDB(checkpointConnector{&f})
			defer db.Close()
			version, err := verifyNativeRuntimeCheckpoint(context.Background(), db)
			if tc.want == 0 {
				if err == nil || version != 0 || f.committed {
					t.Fatal("unreviewed checkpoint admitted")
				}
				return
			}
			if err != nil || version != tc.want || !f.committed {
				t.Fatalf("verified checkpoint metadata differs: version=%d err=%v committed=%v", version, err, f.committed)
			}
		})
	}
}
