package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/logging"
	"Bills-Backend/monnify"
	"Bills-Backend/order"
	"Bills-Backend/reply"
)

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)

	cfg, err := LoadConfig()
	if err != nil {
		log.Error("Config", "detail", err.Error())
		os.Exit(1)
	}

	serve(cfg, log)
}

func serve(cfg Config, log *slog.Logger) {
	store, err := order.OpenStore(cfg.DatabasePath)
	if err != nil {
		log.Error("Database", "detail", "could not open: "+err.Error())
		os.Exit(1)
	}
	defer store.Close()

	payments := monnify.New(cfg.MonnifyBaseURL, cfg.MonnifyAPIKey, cfg.MonnifySecretKey, cfg.MonnifyContractCode)
	vendor := abs.New(cfg.ABSBaseURL, cfg.ABSAPIKey, cfg.ABSSandbox)
	payments.Log, vendor.Log = log, log

	api := &order.API{
		Service: &order.Service{
			Store:  store,
			Pay:    payments,
			Vendor: vendor,
			Log:    log,
		},
		MonnifySecret:          cfg.MonnifySecretKey,
		VerifyMonnifySignature: cfg.IsProduction(),
		ABSWebhookSecret:       cfg.ABSWebhookSecret,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		reply.OKWithMessage(w, http.StatusOK, "ok", nil)
	})
	mux.Handle("/", api.Routes())
	// Direct access to the providers. Same in every environment; the ABS sandbox
	// setting decides whether purchases are real. Protected by the admin token.
	direct := http.NewServeMux()
	vendor.RegisterDirectRoutes(direct)
	payments.RegisterDirectRoutes(direct)
	mux.Handle("/abs/", requireAdmin(cfg.AdminToken, direct))
	mux.Handle("/monnify/", requireAdmin(cfg.AdminToken, direct))

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: logging.Requests(log, mux), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("Server", "detail", fmt.Sprintf("listening on %s · env=%s · abs_sandbox=%t", cfg.HTTPAddr, cfg.Env, cfg.ABSSandbox))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("Server", "detail", err.Error())
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	api.Wait() // let in-flight purchases finish
}
