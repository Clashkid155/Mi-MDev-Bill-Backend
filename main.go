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
	"Bills-Backend/email"
	"Bills-Backend/kora"
	"Bills-Backend/logging"
	"Bills-Backend/monnify"
	"Bills-Backend/order"
	"Bills-Backend/payment"
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

	vendor := abs.New(cfg.ABSBaseURL, cfg.ABSAPIKey, cfg.ABSSandbox)
	vendor.Log = log
	// Only the chosen provider is built; the others' keys can stay empty.
	var payments payment.Provider
	var direct *http.ServeMux
	switch cfg.PaymentProvider {
	case "kora":
		k := kora.New(cfg.KoraBaseURL, cfg.KoraSecretKey)
		k.Log = log
		payments = k
		direct = http.NewServeMux()
		k.RegisterDirectRoutes(direct)
	default:
		m := monnify.New(cfg.MonnifyBaseURL, cfg.MonnifyAPIKey, cfg.MonnifySecretKey, cfg.MonnifyContractCode)
		m.Log = log
		payments = m
		direct = http.NewServeMux()
		m.RegisterDirectRoutes(direct)
	}
	log.Info("Config", "detail", "payments by "+cfg.PaymentProvider)

	// Mailer is nil (and the purchase-successful email skipped) until
	// ZEPTOMAIL_TOKEN and MAIL_FROM_EMAIL are both set; see Config.MailConfigured.
	var mailer *email.Client
	if cfg.MailConfigured() {
		mailer = email.New(email.Settings{
			BaseURL: cfg.MailBaseURL, Token: cfg.MailToken, FromEmail: cfg.MailFromEmail,
			FromName: cfg.MailFromName, BounceAddress: cfg.MailBounceAddress, Sandbox: cfg.MailSandbox,
		})
		mailer.Log = log
		if cfg.MailSandbox {
			log.Info("Config", "detail", "email in sandbox: receipts are logged, not sent (MAIL_SANDBOX=false to send)")
		}
	} else {
		log.Info("Config", "detail", "email disabled: ZEPTOMAIL_TOKEN/MAIL_FROM_EMAIL not set")
	}

	api := &order.API{
		Service: &order.Service{
			Store:          store,
			Pay:            payments,
			Vendor:         vendor,
			Log:            log,
			Mailer:         mailer,
			MinPaymentKobo: cfg.MinPaymentKobo,
			Pricing:        cfg.Pricing(),
			FeeWarnPercent: cfg.PaymentFeeWarnPercent,
		},
		MonnifySecret:          cfg.MonnifySecretKey,
		VerifyMonnifySignature: cfg.IsProduction(),
		ABSWebhookSecret:       cfg.ABSWebhookSecret,
		KoraSecret:             cfg.KoraSecretKey,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		reply.OKWithMessage(w, http.StatusOK, "ok", nil)
	})
	mux.Handle("/", api.Routes())
	// Direct access to the providers, for curl and Postman. These can spend real
	// money, so they are only served while nothing real is at stake.
	if cfg.RealMoney() {
		log.Info("Server", "detail", "direct /abs, /monnify and /email endpoints are off while real money is in use")
	} else {
		vendor.RegisterDirectRoutes(direct)
		if mailer != nil {
			mailer.RegisterDirectRoutes(direct)
		}
		for _, prefix := range []string{"/abs/", "/monnify/", "/kora/", "/email/"} {
			mux.Handle(prefix, requireAdmin(cfg.AdminToken, direct))
		}
	}
	mux.Handle("/admin/", requireAdmin(cfg.AdminToken, api.AdminRoutes()))

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: logging.Requests(log, mux), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go api.Sweep(ctx, 5*time.Minute)

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
