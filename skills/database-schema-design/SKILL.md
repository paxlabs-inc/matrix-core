---
name: database-schema-design
description: Relational schema design playbook — normalization, keys and identifiers, constraints, indexing, migrations, and modeling common relationships. Use when designing or evolving a database schema.
origin: Matrix
---

# Database Schema Design Playbook

Design and evolve a relational schema that the database itself keeps correct. Push
invariants into constraints, not application code — the schema is the last line that cannot
be bypassed by a buggy service or an ad-hoc script.

Use when: modeling a new domain, adding tables, or reviewing a schema/migration before it
ships.

## Principles

- **Normalize first (to 3NF), denormalize only on evidence.** Duplicate data is a
  consistency bug waiting to happen. Denormalize for a measured read pattern, and say so.
- **Constraints over convention.** `NOT NULL`, `UNIQUE`, `CHECK`, and foreign keys make bad
  states unrepresentable. If a rule can live in the schema, it lives in the schema.
- **Every table has a primary key.** No exceptions.
- **Model the domain, not the current UI.** Screens change; the entities and their
  relationships are more durable.

## Keys and identifiers

- **Surrogate PK** (`bigint identity` or UUID) for internal joins; stable and opaque.
- Prefer **UUIDv7** (or `bigint identity`) over UUIDv4 for primary keys — v7 is
  time-ordered, so inserts stay index-friendly instead of fragmenting the B-tree.
- Add a **natural/business key** as a separate `UNIQUE` constraint (email, slug, SKU) — do
  not overload it as the PK.
- Never expose a sequential PK where enumeration is a risk; expose the UUID or a slug.

## Columns and types

- Tightest type that fits: `text` over arbitrary `varchar(n)` in Postgres; real `timestamptz`
  (always store UTC) over strings; `numeric` for money, never `float`.
- Enumerations: a `CHECK` constraint or a lookup table over a free-text column. Native
  `enum` types are rigid to alter — prefer a lookup table when the set evolves.
- `NOT NULL` by default; allow NULL only when "unknown/absent" is a real, distinct state.
  Do not use NULL as a sentinel for a default.

## Relationships

- **One-to-many:** FK on the child pointing at the parent PK, with `ON DELETE` chosen
  deliberately (`RESTRICT`/`CASCADE`/`SET NULL`) — the default is not automatic.
- **Many-to-many:** a junction table with a composite PK `(a_id, b_id)` plus its own
  attributes (e.g. `role`, `added_at`).
- **Soft delete:** a `deleted_at timestamptz` plus partial unique indexes
  (`WHERE deleted_at IS NULL`) if you still need uniqueness among live rows. Prefer hard
  delete unless retention/audit requires otherwise.
- Add `created_at`/`updated_at timestamptz` to entity tables as a matter of course.

## Indexing

- Index every foreign key you join or filter on — this is the most common missing index.
- Composite index column order follows query predicates: equality columns first, then range/
  sort. Match the index to the actual `WHERE`/`ORDER BY`.
- Use partial and expression indexes for skewed predicates (`WHERE status = 'active'`).
- Enforce business uniqueness with a `UNIQUE` index, not application checks (which race).
- Do not index blindly — every index is write cost. Index for the queries you have.

## Migrations

- Migrations are **forward-only, versioned, reviewed** artifacts in the repo (Prisma
  Migrate, Drizzle, Alembic, golang-migrate, Flyway — match the stack). Never edit a table by
  hand in production.
- **Expand/contract** for zero-downtime changes: add the new nullable column and backfill,
  deploy code that writes both, switch reads, then drop the old column in a later migration.
  Never rename-in-place under live traffic.
- Adding a `NOT NULL` column to a big table: add nullable → backfill in batches → set
  `NOT NULL`. A bare `NOT NULL DEFAULT` can lock/rewrite the table.
- Create indexes concurrently (`CREATE INDEX CONCURRENTLY` in Postgres) on hot tables.

## Testing and validation

- Run migrations up **and down** (or up + a fresh rebuild) in CI against a real database of
  the same engine — not SQLite standing in for Postgres.
- Assert constraints actually reject bad data with tests (duplicate insert fails, FK
  violation fails).
- `EXPLAIN (ANALYZE)` the queries that matter before declaring an index done.

## Common pitfalls

- No FK constraints "for flexibility" — you traded referential integrity for orphan rows.
- `float` for money — rounding corruption.
- Storing local time or naive timestamps — always `timestamptz` in UTC.
- Unindexed foreign keys — slow joins and slow cascade deletes.
- EAV (entity-attribute-value) as a shortcut for a real schema — unqueryable and
  unconstrained; model the entities.
- Editing production schema by hand instead of a reviewed migration.
