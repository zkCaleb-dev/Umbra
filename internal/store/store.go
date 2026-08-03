// Package store owns all Postgres access: embedded migrations, the
// per-ledger atomic write, and the read queries the API serves.
//
// The core guarantee lives in WriteLedger: a ledger's raw events, its
// derived rows and the cursor advance commit in ONE transaction. A crash
// at any point either persists the whole ledger or none of it, so
// ingestion is exactly-once without any broker or dedupe machinery.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and applies pending migrations.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping verifies connectivity (used by /readyz).
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// migrate applies embedded migrations in filename order, tracking them in
// schema_migrations. Each migration runs in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a numeric version", name)
		}
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("checking migration %d: %w", version, err)
		}
		if exists {
			continue
		}
		sql, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("beginning migration %d: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("applying migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations(version) VALUES($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("recording migration %d: %w", version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing migration %d: %w", version, err)
		}
	}
	return nil
}

// ===== Write path =====

// RawEvent is one contract event ready for persistence.
type RawEvent struct {
	ID             string
	Ledger         uint32
	LedgerClosedAt time.Time
	TxHash         string
	TxIndex        uint32
	EventIndex     uint32
	ContractID     string
	Name           string
	TopicsJSON     []byte // JSON array
	DataJSON       []byte // JSON value or nil
	RawXDR         []byte
}

// Leaf is a derived pool_leaves row.
type Leaf struct {
	PoolID          string
	LeafIndex       uint32
	Commitment      string // 0x hex
	EncryptedOutput []byte
	Ledger          uint32
}

// Nullifier is a derived pool_nullifiers row.
type Nullifier struct {
	PoolID    string
	Nullifier string // 0x hex
	Ledger    uint32
}

// RegistryKey is a derived registry_keys row.
type RegistryKey struct {
	RegistryID    string
	Owner         string
	EncryptionKey []byte
	NoteKey       []byte
	Ledger        uint32
}

// Derived groups the derived rows produced from one ledger's events.
type Derived struct {
	Leaves     []Leaf
	Nullifiers []Nullifier
	Registry   []RegistryKey
}

// WriteLedger atomically persists a ledger: raw events + derived rows +
// cursor. Safe to call for every ledger including empty ones (cursor
// still advances). Re-processing a ledger is a no-op thanks to the
// deterministic ids and ON CONFLICT clauses.
func (s *Store) WriteLedger(ctx context.Context, network string, ledger uint32,
	ledgerHash string, events []RawEvent, d Derived) error {

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("beginning ledger tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	for _, ev := range events {
		if _, err := tx.Exec(ctx, `
			INSERT INTO events (id, network, ledger, ledger_closed_at, tx_hash,
				tx_index, event_index, contract_id, event_name, topics, data, raw_xdr)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (id) DO NOTHING`,
			ev.ID, network, int64(ev.Ledger), ev.LedgerClosedAt, ev.TxHash,
			int32(ev.TxIndex), int32(ev.EventIndex), ev.ContractID, ev.Name,
			ev.TopicsJSON, ev.DataJSON, ev.RawXDR); err != nil {
			return fmt.Errorf("inserting event %s: %w", ev.ID, err)
		}
	}
	for _, l := range d.Leaves {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pool_leaves (pool_id, leaf_index, commitment, encrypted_output, ledger)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT (pool_id, leaf_index) DO NOTHING`,
			l.PoolID, int64(l.LeafIndex), l.Commitment, l.EncryptedOutput, int64(l.Ledger)); err != nil {
			return fmt.Errorf("inserting leaf %d of %s: %w", l.LeafIndex, l.PoolID, err)
		}
	}
	for _, n := range d.Nullifiers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pool_nullifiers (pool_id, nullifier, ledger)
			VALUES ($1,$2,$3) ON CONFLICT (pool_id, nullifier) DO NOTHING`,
			n.PoolID, n.Nullifier, int64(n.Ledger)); err != nil {
			return fmt.Errorf("inserting nullifier of %s: %w", n.PoolID, err)
		}
	}
	for _, r := range d.Registry {
		if _, err := tx.Exec(ctx, `
			INSERT INTO registry_keys (registry_id, owner, encryption_key, note_key, ledger)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (registry_id, owner) DO UPDATE
			SET encryption_key = EXCLUDED.encryption_key,
			    note_key = EXCLUDED.note_key,
			    ledger = EXCLUDED.ledger
			WHERE EXCLUDED.ledger >= registry_keys.ledger`,
			r.RegistryID, r.Owner, r.EncryptionKey, r.NoteKey, int64(r.Ledger)); err != nil {
			return fmt.Errorf("upserting registry key for %s: %w", r.Owner, err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cursor (network, last_ledger, last_ledger_hash, updated_at)
		VALUES ($1,$2,$3,now())
		ON CONFLICT (network) DO UPDATE
		SET last_ledger = EXCLUDED.last_ledger,
		    last_ledger_hash = EXCLUDED.last_ledger_hash,
		    updated_at = now()`,
		network, int64(ledger), ledgerHash); err != nil {
		return fmt.Errorf("advancing cursor: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing ledger %d: %w", ledger, err)
	}
	return nil
}

// RecordGap persists evidence of a range we could not ingest.
func (s *Store) RecordGap(ctx context.Context, network string, from, to uint32, reason string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO gaps (network, from_ledger, to_ledger, reason) VALUES ($1,$2,$3,$4)`,
		network, int64(from), int64(to), reason)
	return err
}

// Cursor returns the persisted cursor, or ok=false when none exists yet.
func (s *Store) Cursor(ctx context.Context, network string) (ledger uint32, hash string, ok bool, err error) {
	var l int64
	err = s.pool.QueryRow(ctx,
		`SELECT last_ledger, last_ledger_hash FROM cursor WHERE network=$1`, network,
	).Scan(&l, &hash)
	if err == pgx.ErrNoRows {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return uint32(l), hash, true, nil
}
