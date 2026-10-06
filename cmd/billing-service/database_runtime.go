package main

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Billing checks its owned catalog and the common native checkpoint without
// SQL repair or business-row reads. Accounts checks the full compiled catalog;
// Toolkit must bind both services to the same reviewed connection and final
// full-business receipt. This check does not claim full business equality.
func verifyNativeRuntime(ctx context.Context, db *sql.DB) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return errors.New("Billing native runtime schema unavailable")
	}
	defer tx.Rollback()
	var database string
	var pgVersion, count, version int
	var dirty bool
	if tx.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&database, &pgVersion) != nil || database != "account" || pgVersion < 170000 || pgVersion >= 180000 {
		return errors.New("Billing native runtime requires PostgreSQL17 account database")
	}
	if tx.QueryRowContext(ctx, `SELECT count(*),coalesce(min(version),0),coalesce(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty) != nil || count != 1 || version != 2026100701 || dirty {
		return errors.New("Billing native runtime requires the exact clean checkpoint")
	}
	if tx.QueryRowContext(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') AND c.relname NOT IN ('schema_migrations','system_release_checkpoints')`).Scan(&count) != nil || count != 53 {
		return errors.New("Billing native runtime shared table scope differs")
	}
	expected := map[string]string{"id": "uuid:false", "provider": "character varying(50):false", "account_id": "character varying(100):false",
		"service_name": "character varying(100):false", "region": "character varying(100):false", "usage_start_time": "timestamp with time zone:false",
		"usage_end_time": "timestamp with time zone:false", "cost_amount": "double precision:false", "currency": "character varying(10):false",
		"usage_quantity": "double precision:true", "usage_unit": "character varying(50):true", "created_at": "timestamp with time zone:false"}
	rows, err := tx.QueryContext(ctx, `SELECT attname,format_type(atttypid,atttypmod)||':'||(NOT attnotnull)::text FROM pg_attribute WHERE attrelid=to_regclass('public.cloud_vendor_costs') AND attnum>0 AND NOT attisdropped`)
	if err != nil {
		return errors.New("Billing owned native catalog unavailable")
	}
	for rows.Next() {
		var name, definition string
		if rows.Scan(&name, &definition) != nil || expected[name] != definition {
			rows.Close()
			return errors.New("Billing owned native column differs")
		}
		delete(expected, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(expected) != 0 {
		return errors.New("Billing owned native scope incomplete")
	}
	var primaryKey string
	if tx.QueryRowContext(ctx, `SELECT string_agg(a.attname,',' ORDER BY k.ord) FROM pg_constraint c CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY k(num,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num WHERE c.conrelid=to_regclass('public.cloud_vendor_costs') AND c.contype='p'`).Scan(&primaryKey) != nil || primaryKey != "id" {
		return errors.New("Billing owned native key differs")
	}
	var uniqueKey string
	if tx.QueryRowContext(ctx, `SELECT string_agg(a.attname,',' ORDER BY k.ord) FROM pg_constraint c CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY k(num,ord) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.num WHERE c.conrelid=to_regclass('public.cloud_vendor_costs') AND c.contype='u' AND c.conname='uq_cloud_vendor_cost_period' AND c.convalidated`).Scan(&uniqueKey) != nil || uniqueKey != "provider,account_id,service_name,region,usage_start_time" {
		return errors.New("Billing owned native upsert key differs")
	}
	var visible bool
	if tx.QueryRowContext(ctx, `SELECT has_table_privilege(current_user,'public.cloud_vendor_costs','SELECT') AND NOT row_security_active('public.cloud_vendor_costs')`).Scan(&visible) != nil || !visible {
		return errors.New("Billing owned native visibility unproven")
	}
	return tx.Commit()
}
