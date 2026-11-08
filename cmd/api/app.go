package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"time"

	"github.com/Cadoranee/Doverka/internal/clock"
	"github.com/Cadoranee/Doverka/internal/config"
	"github.com/Cadoranee/Doverka/internal/httpapi"
	"github.com/Cadoranee/Doverka/internal/idempotency"
	"github.com/Cadoranee/Doverka/internal/ledger"
	providerclient "github.com/Cadoranee/Doverka/internal/provider/client"
	selection "github.com/Cadoranee/Doverka/internal/provider/select"
	"github.com/Cadoranee/Doverka/internal/quote"
	"github.com/Cadoranee/Doverka/internal/rates"
	"github.com/Cadoranee/Doverka/internal/reconcile"
	"github.com/Cadoranee/Doverka/internal/refdata"
	"github.com/Cadoranee/Doverka/internal/store"
	"github.com/Cadoranee/Doverka/internal/transfer"
	"github.com/Cadoranee/Doverka/internal/webhook"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
)

func run(ctx context.Context, cfg config.Config) error {
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	cat, err := refdata.Load(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load refdata: %w", err)
	}
	providers, err := selection.LoadProviders(filepath.Join(cfg.DataDir, "providers.json"))
	if err != nil {
		return fmt.Errorf("load providers: %w", err)
	}

	clk := clock.System{}
	pool := db.Pool()
	quoteRepo := quote.NewRepo(pool, clk)
	machine := transfer.New(pool, clk)
	led := ledger.New(pool)
	rateSvc := rates.New(rates.NewHTTPSource(cfg.MockProviderURL, nil), clk)
	pc := providerclient.New(cfg.MockProviderURL, cfg.APIPublicURL, nil)

	if err := seedClientBalances(ctx, pool, led, cat); err != nil {
		return fmt.Errorf("seed balances: %w", err)
	}

	exec := transfer.NewExecutor(pool, machine, led, pc, clk)
	transferRunner := transfer.NewRunner(exec, machine, quoteRepo, cat, providers, rateSvc, clk)
	resumeWorker := reconcile.New(pool, machine, led, transferRunner)
	reconcileJob := reconcile.NewJob(pool, machine, led, pc, clk)

	apiHandler := &httpapi.Handler{
		Catalog:      cat,
		Balances:     led,
		Rates:        rateSvc,
		Store:        quoteRepo,
		Clock:        clk,
		DB:           pool,
		Transfers:    machine,
		TransferView: machine,
		Ledger:       led,
		Idempotency:  idempotency.New(pool),
		Runner:       resumeWorker,
		Convert:      rateSvc.Convert,
		Providers:    providers,
	}

	wh := &webhook.Handler{
		Secret:    cfg.ProviderWebhookSecret,
		Pool:      pool,
		Transfers: machine,
		Ledger:    led,
		Clock:     clk,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Print(err)
		}
	})
	mux.Handle("POST /webhooks/{provider_id}", wh.Handler())
	mux.Handle("/", apiHandler.Routes())

	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.HTTPAddr, err)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	go func() {
		if err := resumeWorker.Resume(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("resume worker: %v", err)
		}
	}()
	go func() {
		if err := reconcileJob.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("reconcile job: %v", err)
		}
	}()

	log.Printf("listening on %s", cfg.HTTPAddr)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func seedClientBalances(ctx context.Context, pool *pgxpool.Pool, led *ledger.Ledger, cat *refdata.Catalog) error {
	for _, cl := range cat.Clients() {
		currencies := make([]string, 0, len(cl.Balances))
		for code := range cl.Balances {
			currencies = append(currencies, code)
		}
		sort.Strings(currencies)
		for _, code := range currencies {
			amount := cl.Balances[code]
			cur, ok := cat.Currency(code)
			if !ok {
				return fmt.Errorf("client %s: unknown currency %q", cl.ClientID, code)
			}
			minor, err := decimalToMinor(amount, cur.MinorUnits)
			if err != nil {
				return fmt.Errorf("client %s currency %s: %w", cl.ClientID, code, err)
			}
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			seedErr := led.Seed(ctx, tx, cl.ClientID, code, minor)
			if seedErr != nil {
				_ = tx.Rollback(ctx)
				if errors.Is(seedErr, ledger.ErrAlreadySeeded) {
					continue
				}
				return seedErr
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func decimalToMinor(amount decimal.Decimal, minorUnits int) (int64, error) {
	if minorUnits < 0 {
		return 0, fmt.Errorf("invalid minor_units %d", minorUnits)
	}
	scaled := amount.Shift(int32(minorUnits))
	if !scaled.Equal(scaled.Truncate(0)) {
		return 0, fmt.Errorf("amount %s is not a whole number of minor units", amount.String())
	}
	return scaled.IntPart(), nil
}
