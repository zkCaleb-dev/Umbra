// Package config loads Umbra's configuration from the environment plus a
// deployments file describing which contracts to index.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Network passphrases for the two public Stellar networks.
const (
	PassphraseTestnet = "Test SDF Network ; September 2015"
	PassphrasePubnet  = "Public Global Stellar Network ; September 2015"
)

// ContractKind selects the decoder applied to a contract's events. Every
// kind also stores raw events; "raw" stores ONLY raw events.
type ContractKind string

const (
	KindSPPPool          ContractKind = "spp-pool"
	KindSPPRegistry      ContractKind = "spp-registry"
	KindSPPASPMembership ContractKind = "spp-asp-membership"
	KindSPPASPNonMember  ContractKind = "spp-asp-non-membership"
	// KindToken decodes SEP-41/SAC transfer events into the
	// token_transfers view (queryable per address).
	KindToken ContractKind = "token"
	// KindRaw stores raw events only — works for any Soroban contract.
	KindRaw ContractKind = "raw"
)

// Contract is one entry of the deployments file.
type Contract struct {
	ID          string       `json:"id"`
	Kind        ContractKind `json:"kind"`
	StartLedger uint32       `json:"start_ledger"`
	// Label is a human-readable name surfaced by the API (optional).
	Label string `json:"label,omitempty"`
}

// Deployments is the JSON file listing indexed contracts.
type Deployments struct {
	Network   string     `json:"network"`
	Contracts []Contract `json:"contracts"`
}

// Config is the full runtime configuration.
type Config struct {
	// Network is "testnet" or "pubnet".
	Network string `env:"UMBRA_NETWORK" envDefault:"testnet"`
	// RPCURLs is the ordered failover pool; first entry is primary.
	RPCURLs []string `env:"UMBRA_RPC_URLS" envSeparator:"," envDefault:"https://soroban-testnet.stellar.org"`
	// DatabaseURL is the Postgres DSN.
	DatabaseURL string `env:"DATABASE_URL,notEmpty"`
	// DeploymentsPath points at the contracts JSON file.
	DeploymentsPath string `env:"UMBRA_DEPLOYMENTS" envDefault:"deployments/testnet.json"`
	// APIBind is the HTTP listen address for the read API.
	APIBind string `env:"UMBRA_API_BIND" envDefault:":8080"`
	// LedgerFetchTimeout bounds a single GetLedger call so a hung RPC
	// rotates the pool instead of freezing the loop.
	LedgerFetchTimeout time.Duration `env:"UMBRA_LEDGER_FETCH_TIMEOUT" envDefault:"60s"`
	// GetLedgersLimit is the backend buffer hint (keep small on catch-up:
	// large values multiply response sizes).
	GetLedgersLimit int `env:"UMBRA_GET_LEDGERS_LIMIT" envDefault:"10"`
	// LogClientIP opts INTO logging client addresses on the API. Off by
	// default: sync patterns of privacy wallets are sensitive metadata.
	LogClientIP bool `env:"UMBRA_LOG_CLIENT_IP" envDefault:"false"`

	Deployments Deployments `env:"-"`
}

// Load reads env + deployments file and validates the result.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parsing environment: %w", err)
	}

	raw, err := os.ReadFile(cfg.DeploymentsPath)
	if err != nil {
		return nil, fmt.Errorf("reading deployments file %s: %w", cfg.DeploymentsPath, err)
	}
	if err := json.Unmarshal(raw, &cfg.Deployments); err != nil {
		return nil, fmt.Errorf("parsing deployments file %s: %w", cfg.DeploymentsPath, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Network != "testnet" && c.Network != "pubnet" {
		return fmt.Errorf("UMBRA_NETWORK must be testnet or pubnet, got %q", c.Network)
	}
	if c.Deployments.Network != "" && c.Deployments.Network != c.Network {
		return fmt.Errorf("deployments file is for network %q but UMBRA_NETWORK is %q",
			c.Deployments.Network, c.Network)
	}
	if len(c.Deployments.Contracts) == 0 {
		return fmt.Errorf("deployments file lists no contracts")
	}
	for _, ct := range c.Deployments.Contracts {
		if !strings.HasPrefix(ct.ID, "C") || len(ct.ID) != 56 {
			return fmt.Errorf("contract id %q does not look like a C... strkey", ct.ID)
		}
		switch ct.Kind {
		case KindSPPPool, KindSPPRegistry, KindSPPASPMembership, KindSPPASPNonMember, KindToken, KindRaw:
		default:
			return fmt.Errorf("contract %s has unknown kind %q", ct.ID, ct.Kind)
		}
	}
	if len(c.RPCURLs) == 0 {
		return fmt.Errorf("UMBRA_RPC_URLS must list at least one endpoint")
	}
	return nil
}

// Passphrase returns the network passphrase for the configured network.
func (c *Config) Passphrase() string {
	if c.Network == "pubnet" {
		return PassphrasePubnet
	}
	return PassphraseTestnet
}

// StartLedger is the lowest configured contract start ledger — where
// ingestion begins when the database has no cursor yet.
func (c *Config) StartLedger() uint32 {
	var min uint32
	for _, ct := range c.Deployments.Contracts {
		if ct.StartLedger == 0 {
			continue
		}
		if min == 0 || ct.StartLedger < min {
			min = ct.StartLedger
		}
	}
	return min
}

// ContractKinds returns the id → kind map the pipeline filters with.
func (c *Config) ContractKinds() map[string]ContractKind {
	m := make(map[string]ContractKind, len(c.Deployments.Contracts))
	for _, ct := range c.Deployments.Contracts {
		m[ct.ID] = ct.Kind
	}
	return m
}
