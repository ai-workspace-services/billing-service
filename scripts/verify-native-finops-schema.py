#!/usr/bin/env python3
"""Qualify Billing-owned additive SQL; live execution belongs to Playbooks."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[1]


def require(value, message):
    if not value:
        raise ValueError(message)


def validate():
    manifest = json.loads((ROOT / 'sql/native-finops.manifest.json').read_text())
    require(manifest.get('format') == 1 and manifest.get('owner') == 'ai-workspace-services/billing-service' and
            manifest.get('business_tables') == ['cloud_vendor_costs'] and
            manifest.get('no_business_seeds') is True and manifest.get('database_cutover_approved') is False,
            'Billing native artifact scope differs')
    require(manifest.get('migration_file') == 'sql/migrations/2026100701_cloud_vendor_costs.up.sql' and
            manifest.get('reference_file') == 'sql/billing-service-schema.sql' and
            manifest.get('expected_schema_version') == 2026100601 and manifest.get('target_schema_version') == 2026100701,
            'Reviewed bounded migration versions/paths differ')
    migration = (ROOT / manifest['migration_file']).read_bytes()
    reference = (ROOT / manifest['reference_file']).read_bytes()
    require(hashlib.sha256(migration).hexdigest() == manifest['migration_sha256'] and
            hashlib.sha256(reference).hexdigest() == manifest['reference_sha256'], 'Billing native source or SQL digest differs')
    reference_body = reference.decode()[reference.decode().index('CREATE TABLE IF NOT EXISTS public.cloud_vendor_costs'):].strip()
    reference_body = reference_body.replace('CREATE TABLE IF NOT EXISTS', 'CREATE TABLE').replace('CREATE INDEX IF NOT EXISTS', 'CREATE INDEX')
    reference_body = '\n'.join(line.rstrip() for line in reference_body.splitlines())
    require(migration.decode().split('\n', 1)[1].strip() == reference_body,
            'Billing native fields differ from service-owned current reference')
    require(re.findall(r'CREATE TABLE public\.([a-z_]+)', migration.decode()) == ['cloud_vendor_costs'] and
            not re.search(r'(?im)^\s*(INSERT|UPDATE|DELETE|DROP|TRUNCATE|GRANT|REVOKE|ALTER|\\)', migration.decode()) and
            'IF NOT EXISTS' not in migration.decode(), 'Native artifact must add only its absent owned table without seeds or reset')
    return manifest, migration


def psql(database, query=None, sql=None, expected_failure=False):
    command = ['psql', '-XAtq', '-v', 'ON_ERROR_STOP=1', '--dbname', database]
    if query is not None:
        command += ['-c', query]
    if sql is not None:
        command += ['--single-transaction', '-f', '-']
    result = subprocess.run(command, input=sql, text=True, capture_output=True, timeout=30)
    require((result.returncode != 0) if expected_failure else (result.returncode == 0),
            'Disposable Billing schema qualification query failed')
    return result.stdout.strip()


def postgres_check(migration):
    require(os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted' and
            os.environ.get('PGHOST') in ('127.0.0.1', 'localhost') and os.environ.get('PGPORT') == '5432' and
            os.environ.get('PGUSER') == 'postgres', 'PostgreSQL qualification is restricted to disposable hosted CI loopback')
    require(re.fullmatch(r'17[0-9]{4}', psql('postgres', 'SHOW server_version_num')), 'PostgreSQL 17 qualification required')
    database = 'billing_native_fixture_' + uuid.uuid4().hex
    psql('postgres', 'CREATE DATABASE ' + database)
    try:
        psql(database, sql=migration.decode())
        require(psql(database, "SELECT string_agg(tablename, ',' ORDER BY tablename) FROM pg_tables WHERE schemaname='public'") == 'cloud_vendor_costs',
                'Native SQL changed shared Accounts schema scope')
        expected = ['id', 'provider', 'account_id', 'service_name', 'region', 'usage_start_time', 'usage_end_time',
                    'cost_amount', 'currency', 'usage_quantity', 'usage_unit', 'created_at']
        actual = psql(database, "SELECT string_agg(column_name, ',' ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema='public' AND table_name='cloud_vendor_costs'")
        require(actual.split(',') == expected, 'Native Billing column scope differs')
        require(psql(database, 'SELECT count(*) FROM cloud_vendor_costs') == '0', 'Native SQL created business rows')
        # No IF NOT EXISTS: a second application must refuse instead of silently
        # accepting a pre-existing table with unknown shape.
        psql(database, sql=migration.decode(), expected_failure=True)
        psql(database, "INSERT INTO cloud_vendor_costs(provider,account_id,service_name,usage_start_time,usage_end_time,cost_amount) VALUES ('fixture','disposable','compute','2026-10-01','2026-10-02',1.25)")
        psql(database, "INSERT INTO cloud_vendor_costs(provider,account_id,service_name,usage_start_time,usage_end_time,cost_amount) VALUES ('fixture','disposable','compute','2026-10-01','2026-10-02',2.5) ON CONFLICT ON CONSTRAINT uq_cloud_vendor_cost_period DO UPDATE SET cost_amount=EXCLUDED.cost_amount")
        require(psql(database, 'SELECT count(*) || \':\' || max(cost_amount) FROM cloud_vendor_costs') == '1:2.5',
                'Native Billing upsert uniqueness differs')
        psql(database, "INSERT INTO cloud_vendor_costs(provider,account_id,service_name,usage_start_time,usage_end_time,cost_amount) VALUES ('fixture',NULL,'compute','2026-10-01','2026-10-02',1)", expected_failure=True)
        require(psql(database, "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname IN ('idx_cloud_vendor_costs_time','idx_cloud_vendor_costs_provider')") == '2',
                'Native Billing query indexes are missing')
    finally:
        psql('postgres', 'DROP DATABASE ' + database)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--postgres-check', action='store_true')
    args = parser.parse_args()
    manifest, migration = validate()
    if args.postgres_check:
        postgres_check(migration)
    print(json.dumps({'kind': 'ci_schema_qualification' if args.postgres_check else 'source_contract_only',
        'migration_sha256': manifest['migration_sha256'], 'expected_schema_version': manifest['expected_schema_version'],
        'target_schema_version': manifest['target_schema_version'], 'business_tables': manifest['business_tables'],
        'live_database_initialized': False, 'database_cutover_approved': False}, sort_keys=True))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, KeyError, subprocess.TimeoutExpired):
        print('Billing native schema qualification refused; raw database output withheld.')
        raise SystemExit(1)
