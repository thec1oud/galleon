package coordpg

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/thec1oud/galleon/errs"
)

// schemaVersion is the version of the tables this code reads and writes.
// A change to them raises it, and adds to install the step that upgrades
// a schema of the version before.
const schemaVersion = 1

// queries are the backend's statements, on its schema's tables.
type queries struct {
	schema, meta, leases, members, kv, history, counters string
}

func newQueries(schema string) queries {
	return queries{
		schema:   ident(schema),
		meta:     ident(schema, "meta"),
		leases:   ident(schema, "leases"),
		members:  ident(schema, "members"),
		kv:       ident(schema, "kv"),
		history:  ident(schema, "kv_history"),
		counters: ident(schema, "counters"),
	}
}

// sql replaces the table names in a statement: {meta}, {leases} and so on.
func (q queries) sql(s string) string {
	return strings.NewReplacer("{schema}", q.schema, "{meta}", q.meta, "{leases}", q.leases, "{members}", q.members,
		"{kv}", q.kv, "{history}", q.history, "{counters}", q.counters).Replace(s)
}

// The tables. meta has one row: the schema's version, the store's epoch,
// and the key-value store's last revision. A lease row is never deleted,
// so its token outlives every holder; holder is null while it is free.
const createSchema = `
CREATE SCHEMA IF NOT EXISTS {schema};
CREATE TABLE {meta} (
	one boolean PRIMARY KEY DEFAULT true CHECK (one),
	version integer NOT NULL,
	epoch text NOT NULL,
	kv_revision bigint NOT NULL DEFAULT 0
);
CREATE TABLE {leases} (
	key text PRIMARY KEY,
	token bigint NOT NULL,
	holder text,
	node text,
	expires_at timestamptz
);
CREATE TABLE {members} (
	node text PRIMARY KEY,
	roles text[] NOT NULL,
	expires_at timestamptz NOT NULL
);
CREATE TABLE {kv} (
	key text PRIMARY KEY,
	value bytea NOT NULL,
	revision bigint NOT NULL,
	expires_at timestamptz
);
CREATE INDEX kv_expires_at ON {kv} (expires_at) WHERE expires_at IS NOT NULL;
CREATE TABLE {history} (
	revision bigint PRIMARY KEY,
	key text NOT NULL,
	value bytea,
	deleted boolean NOT NULL,
	at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE {counters} (
	key text NOT NULL,
	window_us bigint NOT NULL,
	window_start bigint NOT NULL,
	value bigint NOT NULL,
	PRIMARY KEY (key, window_us)
);
`

// install creates the schema if it doesn't exist, and returns the store's
// epoch. Nodes starting together take turns, under a transaction-level
// advisory lock on the schema's name, so only one creates it.
func (b *Backend) install(ctx context.Context) (string, error) {
	var epoch string
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "galleon/coord_pg/install/"+b.cfg.Schema); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, b.q.meta).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(ctx, b.q.sql(createSchema)); err != nil {
				return err
			}
			epoch = "pg-" + randomHex(16)
			_, err := tx.Exec(ctx, b.q.sql(`INSERT INTO {meta} (version, epoch) VALUES ($1, $2)`), schemaVersion, epoch)
			return err
		}
		var version int
		if err := tx.QueryRow(ctx, b.q.sql(`SELECT version, epoch FROM {meta}`)).Scan(&version, &epoch); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("coordpg: %s has no row: the schema is damaged", b.q.meta)
			}
			return err
		}
		if version > schemaVersion {
			return errs.New(errs.FailedPrecondition, CodeSchemaNewer,
				fmt.Sprintf("schema %s is at version %d, newer than this node's %d: upgrade the node", b.cfg.Schema, version, schemaVersion))
		}
		return nil
	})
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) {
			return "", err
		}
		return "", b.fail(ctx, fmt.Errorf("coordpg: installing schema %s: %w", b.cfg.Schema, err))
	}
	return epoch, nil
}
