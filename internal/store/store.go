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

// TokenTransfer is a derived token_transfers row (SEP-41/SAC transfer).
type TokenTransfer struct {
	EventID string
	TokenID string
	Ledger  uint32
	TxHash  string
	From    string
	To      string
	Amount  string // i128 as decimal string
}

// CTEvent is a derived ct_events row (OpenZeppelin Confidential Token).
type CTEvent struct {
	EventID      string
	TokenID      string
	Ledger       uint32
	TxHash       string
	Kind         string
	Addresses    []string
	AmountPublic *string
	Payload      []byte // JSON ciphertext material, nil when none
}

// Derived groups the derived rows produced from one ledger's events.
type Derived struct {
	Leaves     []Leaf
	Nullifiers []Nullifier
	Registry   []RegistryKey
	Transfers  []TokenTransfer
	CTEvents   []CTEvent
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

	if err := writeRowsTx(ctx, tx, network, events, d); err != nil {
		return err
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

// writeRows persists events + derived rows in their own transaction,
// leaving the cursor alone (backfill / re-derivation write path).
func (s *Store) writeRows(ctx context.Context, network string, events []RawEvent, d Derived) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("beginning rows tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := writeRowsTx(ctx, tx, network, events, d); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing rows: %w", err)
	}
	return nil
}

// writeRowsTx runs the idempotent inserts inside the caller's tx.
func writeRowsTx(ctx context.Context, tx pgx.Tx, network string, events []RawEvent, d Derived) error {
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
	for _, t := range d.Transfers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO token_transfers (event_id, token_id, ledger, tx_hash, from_addr, to_addr, amount)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (event_id) DO NOTHING`,
			t.EventID, t.TokenID, int64(t.Ledger), t.TxHash, t.From, t.To, t.Amount); err != nil {
			return fmt.Errorf("inserting transfer %s: %w", t.EventID, err)
		}
	}
	for _, c := range d.CTEvents {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ct_events (event_id, token_id, ledger, tx_hash, kind, addresses, amount_public, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (event_id) DO NOTHING`,
			c.EventID, c.TokenID, int64(c.Ledger), c.TxHash, c.Kind, c.Addresses,
			c.AmountPublic, c.Payload); err != nil {
			return fmt.Errorf("inserting ct event %s: %w", c.EventID, err)
		}
	}
	return nil
}

// RecordGap persists evidence of a range we could not ingest.
// Idempotent per (network, range): re-recording a known gap is a no-op,
// so retry loops can never flood the evidence table.
func (s *Store) RecordGap(ctx context.Context, network string, from, to uint32, reason string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO gaps (network, from_ledger, to_ledger, reason) VALUES ($1,$2,$3,$4)
		ON CONFLICT (network, from_ledger, to_ledger) DO NOTHING`,
		network, int64(from), int64(to), reason)
	return err
}

// ResolveGapsWithin deletes gap evidence fully contained in [from, to] —
// called after an archive replay actually recovers that history, at
// which point the evidence would be a false claim of absence.
func (s *Store) ResolveGapsWithin(ctx context.Context, network string, from, to uint32) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM gaps
		WHERE network=$1 AND from_ledger>=$2 AND to_ledger<=$3`,
		network, int64(from), int64(to))
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
