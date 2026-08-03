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

	"github.com/Trustless-Work/umbra/internal/api"
	"github.com/Trustless-Work/umbra/internal/config"
	"github.com/Trustless-Work/umbra/internal/ingest"
	"github.com/Trustless-Work/umbra/internal/store"

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

	ing := ingest.New(cfg, st)
	srv := api.New(cfg, st, ing)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ing.Run(gctx) })
	g.Go(func() error { return srv.Run(gctx) })
	return g.Wait()
}
