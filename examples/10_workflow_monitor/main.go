package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/DeluxeOwl/chronicle/workflow/monitor"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sync/errgroup"
)

type Order struct {
	OrderID       string        `json:"orderID"`
	Wait          time.Duration `json:"wait"`
	AwaitShipment bool          `json:"awaitShipment"`
	Fail          bool          `json:"fail"`
}

type Fulfillment struct {
	Receipt  string `json:"receipt"`
	Tracking string `json:"tracking,omitempty"`
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("workflow example stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	database := flag.String("db", "workflow-demo.db", "SQLite database path")
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	seed := flag.Bool("seed", true, "create sample workflows on every start")
	workers := flag.Int("workers", 4, "number of concurrent workers")
	flag.Parse()
	if *workers <= 0 {
		return errors.New("workers must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	path, err := filepath.Abs(*database)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=on")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(ctx, db)
	if err != nil {
		return err
	}
	wf := registerOrder(runner, logger)
	log, err := eventlog.NewSqlite(db)
	if err != nil {
		return err
	}
	inspector, err := workflow.NewInspector(log)
	if err != nil {
		return err
	}
	handler, err := monitor.NewHandler(inspector)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/workflows/", http.StripPrefix("/workflows", handler))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(
			[]byte(
				`<!doctype html><html lang="en"><meta charset="utf-8"><title>Chronicle demo</title><a href="/workflows/">Open workflow monitor</a></html>`,
			),
		)
	})
	if *seed {
		if err := seedOrders(ctx, wf, runner); err != nil {
			return err
		}
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}
	group, runCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return runner.RunWorkers(
			runCtx,
			*workers,
			workflow.WorkerOptions{PollInterval: 0, LeaseExtendInterval: 0},
		)
	})
	group.Go(func() error {
		logger.Info(
			"workflow monitor listening",
			"url",
			"http://"+*addr+"/workflows/",
			"database",
			path,
			"workers",
			*workers,
		)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	group.Go(func() error {
		<-runCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	})
	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func registerOrder(
	runner *workflow.Runner,
	logger *slog.Logger,
) *workflow.Workflow[Order, Fulfillment] {
	return workflow.New(
		runner,
		"fulfill-order",
		func(wctx *workflow.Context, order *Order) (*Fulfillment, error) {
			receipt, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				// An external payment service must deduplicate this stable key.
				idempotencyKey := string(wctx.InstanceID()) + ":charge-payment"
				logger.InfoContext(
					ctx,
					"charge payment",
					"orderID",
					order.OrderID,
					"idempotencyKey",
					idempotencyKey,
				)
				if order.Fail {
					return "", errors.New("payment service rejected the order")
				}
				return "receipt-" + order.OrderID, nil
			}, workflow.WithStepName("Charge payment"))
			if err != nil {
				return nil, err
			}
			if order.Wait > 0 {
				if err := workflow.Sleep(wctx, order.Wait, workflow.WithStepName("Prepare shipment")); err != nil {
					return nil, err
				}
			}
			var tracking string
			if order.AwaitShipment {
				tracking, err = workflow.WaitForEvent[string](
					wctx,
					"shipment:"+order.OrderID,
					workflow.WaitForEventOptions{Name: "Await carrier", Timeout: time.Hour},
				)
				if err != nil {
					return nil, err
				}
			}
			if err := workflow.Step2(wctx, func(ctx context.Context) error {
				logger.InfoContext(ctx, "send receipt", "orderID", order.OrderID)
				return nil
			}, workflow.WithStepName("Send receipt")); err != nil {
				return nil, err
			}
			return &Fulfillment{Receipt: receipt, Tracking: tracking}, nil
		},
	)
}

func seedOrders(
	ctx context.Context,
	wf *workflow.Workflow[Order, Fulfillment],
	runner *workflow.Runner,
) error {
	runID := strconv.FormatInt(time.Now().UnixNano(), 10)
	orders := []Order{
		{OrderID: "completed-" + runID, Wait: 0, AwaitShipment: false, Fail: false},
		{OrderID: "sleeping-" + runID, Wait: 2 * time.Minute, AwaitShipment: false, Fail: false},
		{OrderID: "waiting-" + runID, Wait: 0, AwaitShipment: true, Fail: false},
		{OrderID: "prepublished-" + runID, Wait: 0, AwaitShipment: true, Fail: false},
		{OrderID: "failed-" + runID, Wait: 0, AwaitShipment: false, Fail: true},
		{OrderID: "retrying-" + runID, Wait: 0, AwaitShipment: false, Fail: true},
		{OrderID: "cancelled-" + runID, Wait: 0, AwaitShipment: false, Fail: false},
	}
	if err := workflow.PublishEvent(ctx, runner, "shipment:"+orders[3].OrderID, "TRACK-42"); err != nil {
		return err
	}
	for index, order := range orders {
		var options []workflow.StartOption
		if index == 5 {
			options = append(
				options,
				workflow.WithRetryStrategy(
					workflow.RetryStrategy{
						MaxAttempts: 3,
						BaseDelay:   time.Minute,
						Factor:      2,
						MaxDelay:    5 * time.Minute,
					},
				),
			)
		}
		id, err := wf.Start(ctx, &order, options...)
		if err != nil {
			return err
		}
		if index == 6 {
			if err := workflow.CancelWorkflow(ctx, runner, id, "cancelled sample order"); err != nil {
				return err
			}
		}
	}
	return nil
}
