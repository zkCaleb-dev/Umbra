package store

import (
	"context"
	"fmt"
)

// Contract registry rows — see migrations/0006_contracts.sql.

// ContractRow is one registered contract.
type ContractRow struct {
	ID          string
	Kind        string
	StartLedger uint32
	Label       string
	Source      string // "config" | "api"
}

// SeedContract upserts a config-sourced contract. The deployments file
// stays authoritative for its own ids (kind/label/start updates land on
// boot), and it reclaims ids that were first registered via API.
func (s *Store) SeedContract(ctx context.Context, c ContractRow) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO contracts (id, kind, start_ledger, label, source)
		VALUES ($1,$2,$3,$4,'config')
		ON CONFLICT (id) DO UPDATE
		SET kind = EXCLUDED.kind, start_ledger = EXCLUDED.start_ledger,
		    label = EXCLUDED.label, source = 'config'`,
		c.ID, c.Kind, int64(c.StartLedger), c.Label)
	if err != nil {
		return fmt.Errorf("seeding contract %s: %w", c.ID, err)
	}
	return nil
}

// InsertContract adds an API-registered contract. Returns false when the
// id already exists (registration is first-writer-wins; config reclaims
// on the next boot regardless).
func (s *Store) InsertContract(ctx context.Context, c ContractRow) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO contracts (id, kind, start_ledger, label, source)
		VALUES ($1,$2,$3,$4,'api')
		ON CONFLICT (id) DO NOTHING`,
		c.ID, c.Kind, int64(c.StartLedger), c.Label)
	if err != nil {
		return false, fmt.Errorf("registering contract %s: %w", c.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListContracts returns every registered contract, config-sourced first,
// then by registration time.
func (s *Store) ListContracts(ctx context.Context) ([]ContractRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, start_ledger, label, source
		FROM contracts
		ORDER BY source, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("listing contracts: %w", err)
	}
	defer rows.Close()

	var out []ContractRow
	for rows.Next() {
		var c ContractRow
		var start int64
		if err := rows.Scan(&c.ID, &c.Kind, &start, &c.Label, &c.Source); err != nil {
			return nil, err
		}
		c.StartLedger = uint32(start)
		out = append(out, c)
	}
	return out, rows.Err()
}

// EventNameCounts returns, per contract, how many events of each name
// are stored — the observability signal for kinds a decoder ignores.
func (s *Store) EventNameCounts(ctx context.Context) (map[string]map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT contract_id, event_name, count(*)
		FROM events GROUP BY contract_id, event_name`)
	if err != nil {
		return nil, fmt.Errorf("counting event names: %w", err)
	}
	defer rows.Close()

	out := map[string]map[string]int64{}
	for rows.Next() {
		var id, name string
		var n int64
		if err := rows.Scan(&id, &name, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int64{}
		}
		out[id][name] = n
	}
	return out, rows.Err()
}
