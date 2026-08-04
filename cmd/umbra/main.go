// Umbra — a durable, verifiable event index for privacy protocols on
// Stellar (SPP privacy pools, confidential tokens).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/zkCaleb-dev/umbra/internal/api"
	"github.com/zkCaleb-dev/umbra/internal/config"
	"github.com/zkCaleb-dev/umbra/internal/ingest"
	"github.com/zkCaleb-dev/umbra/internal/registry"
	"github.com/zkCaleb-dev/umbra/internal/store"
	"github.com/zkCaleb-dev/umbra/internal/view"

	"golang.org/x/sync/errgroup"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("umbra exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// `umbra view` is a pure API client — keys stay local, nothing needs
	// Postgres or a deployments file — so it dispatches before config
	// and store come up.
	if len(os.Args) > 1 && os.Args[1] == "view" {
		return view.Main(ctx, os.Args[2:])
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.Info("umbra starting", "network", cfg.Network,
		"contracts", len(cfg.Deployments.Contracts), "rpc_endpoints", len(cfg.RPCURLs))

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	// The registry is the live watch set: the deployments file seeds it,
	// POST /v1/contracts grows it at runtime, and the contracts table
	// carries API registrations across restarts.
	reg, err := registry.Load(ctx, st, cfg.Deployments.Contracts)
	if err != nil {
		return err
	}

	// `umbra rederive` rebuilds every derived table from the raw event
	// log and exits — run it after adding or fixing a decoder.
	if len(os.Args) > 1 && os.Args[1] == "rederive" {
		return ingest.Rederive(ctx, ingest.RederiveConfig{
			Store: st, Network: cfg.Network, Kinds: reg.Snapshot().Kinds,
		})
	}

	// Derived tables also self-heal on every boot: re-derivation from the
	// raw event log is idempotent and fast at watch-list scale, so a
	// decoder upgrade or a contract kind change becomes zero-ops —
	// redeploy, and history stored under the old configuration turns into
	// first-class derived data. UMBRA_SKIP_REDERIVE=true opts out (for
	// very large datasets where boot time matters more).
	if os.Getenv("UMBRA_SKIP_REDERIVE") != "true" {
		if err := ingest.Rederive(ctx, ingest.RederiveConfig{
			Store: st, Network: cfg.Network, Kinds: reg.Snapshot().Kinds,
		}); err != nil {
			return err
		}
	}

	ing := ingest.New(cfg, st, reg)
	reg.SetOnChange(ing.NotifyChange)
	srv := api.New(cfg, st, ing, reg)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ing.Run(gctx) })
	g.Go(func() error { return srv.Run(gctx) })
	return g.Wait()
}
