# Billing-owned native FinOps schema

Accounts owns the shared users, subscriptions, quota and accounting ledger
schema. Billing additionally owns `cloud_vendor_costs`; its current native
fields are isolated in the reviewed additive migration and checksum manifest.
The source/reference SQL is not run wholesale against the initialized target.

The intended bounded upgrade is clean Accounts native version `2026100601` to
`2026100701`. Live execution must use Playbooks and the prebuilt Accounts
`migratectl migrate` bounded version/hash/lock/timeout interface; Toolkit binds
the immutable reviewed sources and independent production data approval.
Existing table/schema drift refuses before migration. No production SQL is run
by this repository's qualification script or CI; no business seeds are included.

CI validates current source parity, standalone PostgreSQL 17 DDL, zero rows,
field scope, uniqueness/upsert, required values, indexes, and refusal on repeated
DDL. This qualifies SQL only; it does not establish target deployment, full
business copy/equality, or UAT full upgrade/rollback/re-upgrade acceptance.
The complete business contract must cover this table in addition to the 52
Accounts tables before primary cutover. The migration has no destructive down
script; data-bearing rollback requires a reviewed preservation/catchup plan.
