package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "verify-backup" {
		os.Exit(verifyBackup(os.Args[2:], os.Stdout, os.Stderr))
	}
	log := logging.New(os.Stdout, slog.LevelInfo)

	cfg, err := LoadConfig()
	if err != nil {
		log.Error("Config", "detail", err.Error())
		os.Exit(1)
	}

	serve(cfg, log)
}

// verifyBackup is `bills verify-backup <file>`: it opens a snapshot the way a
// restore would, checks it is intact and prints what is in it, so a person can
// see that the numbers match the business. It exits 1 if the file is no good.
func verifyBackup(args []string, out, errOut io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(errOut, "usage: verify-backup <snapshot.db>")
		return 2
	}
	sum, err := order.VerifyBackup(context.Background(), args[0])
	if err != nil {
		fmt.Fprintln(errOut, "BAD BACKUP:", err)
		return 1
	}
	fmt.Fprintf(out, "backup ok: %d orders, %d expenses, %d settings\n", sum.Orders, sum.Expenses, sum.Settings)
	if !sum.LatestOrderAt.IsZero() {
		fmt.Fprintf(out, "latest order: %s\n", sum.LatestOrderAt.Format(time.RFC3339))
	}
	return 0
}

func serve(cfg Config, log *slog.Logger) {
	store, err := order.OpenStore(cfg.DatabasePath)
	if err != nil {
		log.Error("Database", "detail", "could not open: "+err.Error())
		os.Exit(1)
	}
	defer store.Close()

	// The settings the owner can change from the admin API: .env gives the starting
	// values and anything changed through the API (saved in the database) wins.
	settings, err := order.NewSettings(context.Background(), store, cfg.SettingDefaults(), log)
	if err != nil {
		log.Error("Settings", "detail", "could not load: "+err.Error())
		os.Exit(1)
	}

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
			FromName: cfg.MailFromName, BounceAddress: cfg.MailBounceAddress, Sandbox: settings.Bool("mail.sandbox"),
		})
		mailer.Log = log
		if settings.Bool("mail.sandbox") {
			log.Info("Config", "detail", "email in sandbox: receipts are logged, not sent (MAIL_SANDBOX=false to send)")
		}
	} else {
		log.Info("Config", "detail", "email disabled: ZEPTOMAIL_TOKEN/MAIL_FROM_EMAIL not set")
	}

	// Alerts go to the owner's email through the same mailer. Without both an
	// address and a mailer they are only logged.
	var alerts order.Alerter
	var emailAlerts *order.EmailAlerts
	settings.SetCapability("mail", mailer != nil)
	if mailer != nil {
		emailAlerts = order.NewEmailAlerts(mailer, settings.Text("alerts.email"), log)
		alerts = emailAlerts
		// The address and the sandbox switch can be changed from the admin API.
		settings.OnChange(func() {
			mailer.SetSandbox(settings.Bool("mail.sandbox"))
			emailAlerts.SetTo(settings.Text("alerts.email"))
		})
		if settings.Text("alerts.email") != "" {
			log.Info("Config", "detail", "alerts go to "+settings.Text("alerts.email"))
		} else {
			log.Info("Config", "detail", "alerts are only logged: set ADMIN_ALERT_EMAIL (or alerts.email in the admin API) to be emailed")
		}
	} else {
		log.Info("Config", "detail", "alerts are only logged: the email settings are not set")
	}

	api := &order.API{
		Service: &order.Service{
			Store:          store,
			Pay:            payments,
			Vendor:         vendor,
			Log:            log,
			Mailer:         mailer,
			MinPaymentKobo: cfg.MinPaymentKobo,
			Settings:       settings,
			Pricing:        cfg.Pricing(),
			Alerts:         alerts,
			FeeWarnPercent: cfg.PaymentFeeWarnPercent,
		},
		MonnifySecret:          cfg.MonnifySecretKey,
		VerifyMonnifySignature: cfg.RealMoney(),
		ABSWebhookSecret:       cfg.ABSWebhookSecret,
		RequireSignatures:      cfg.RealMoney(),
		WebhookRetention:       time.Duration(cfg.WebhookRetentionDays) * 24 * time.Hour,
		KoraSecret:             cfg.KoraSecretKey,
	}

	// Direct access to the providers, for curl and Postman. These can spend real
	// money, so they are only served while nothing real is at stake.
	var directRoutes http.Handler
	if cfg.RealMoney() {
		log.Info("Server", "detail", "direct /abs, /monnify and /email endpoints are off while real money is in use")
	} else {
		vendor.RegisterDirectRoutes(direct)
		if mailer != nil {
			mailer.RegisterDirectRoutes(direct)
		}
		directRoutes = direct
	}
	log.Info("Config", "detail", fmt.Sprintf("client addresses for rate limits come from X-Forwarded-For: %t", cfg.TrustProxy))

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           routes(cfg, log, api, directRoutes),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // the slowest call out is ABS at 30s
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go api.Sweep(ctx, 5*time.Minute)
	if cfg.BackupDir != "" {
		go api.WatchBackups(ctx, cfg.BackupDir, time.Duration(cfg.BackupEveryMinutes)*time.Minute)
		log.Info("Config", "detail", fmt.Sprintf("database snapshots to %s every %d minutes", cfg.BackupDir, cfg.BackupEveryMinutes))
	} else {
		log.Warn("Config", "detail", "no database snapshots: set BACKUP_DIR (see deploy/README.md)")
	}
	if emailAlerts != nil {
		go api.WatchBalance(ctx,
			func() time.Duration { return time.Duration(settings.Int("alerts.check_minutes")) * time.Minute },
			func() int64 { return settings.Int("alerts.low_balance_naira") * 100 })
	}

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
	if emailAlerts != nil {
		emailAlerts.Wait() // and alerts being sent
	}
}
