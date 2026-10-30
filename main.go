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
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"Bills-Backend/abs"
	"Bills-Backend/email"
	"Bills-Backend/kora"
	"Bills-Backend/logging"
	"Bills-Backend/monnify"
	"Bills-Backend/notify"
	"Bills-Backend/order"
	"Bills-Backend/payment"
	"Bills-Backend/platform"
)

// version is the release this program was built as ("1.0.0", from the git tag; see deploy/version.sh);
// deploy/deploy.sh sets it. When it is not set (go run, or a build from the IDE) buildVersion reads the
// commit Go embedded in the binary.
var version = "dev"

// buildVersion is the version to report: the one deploy/deploy.sh stamped, else the commit Go
// recorded when it built the program ("-uncommitted" if the tree had changes), else "dev".
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	rev, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if modified {
		rev += "-uncommitted"
	}
	return rev
}

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
	// One database for every part of the backend: each package adds its own tables to it.
	db, err := platform.OpenDB(cfg.DatabasePath)
	if err != nil {
		log.Error("Database", "detail", "could not open: "+err.Error())
		os.Exit(1)
	}
	defer db.Close()
	store, err := order.NewStore(db)
	if err != nil {
		log.Error("Database", "detail", "could not set up the bills tables: "+err.Error())
		os.Exit(1)
	}
	shared, err := platform.NewStore(db)
	if err != nil {
		log.Error("Database", "detail", "could not set up the shared tables: "+err.Error())
		os.Exit(1)
	}
	noticeStore, err := notify.NewStore(db)
	if err != nil {
		log.Error("Database", "detail", "could not set up the notices table: "+err.Error())
		os.Exit(1)
	}

	// Events: what happened to orders and the server's warnings and errors, kept for the admin app.
	// Written in the background, so recording one never slows an order down.
	events := platform.NewEventRecorder(shared, log)
	logging.SetSink(log, events.Capture)

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
			Events:         events,
			FeeWarnPercent: cfg.PaymentFeeWarnPercent,
		},
		MonnifySecret:          cfg.MonnifySecretKey,
		VerifyMonnifySignature: cfg.RealMoney(),
		ABSWebhookSecret:       cfg.ABSWebhookSecret,
		RequireSignatures:      cfg.RealMoney(),
		WebhookRetention:       time.Duration(cfg.WebhookRetentionDays) * 24 * time.Hour,
		KoraSecret:             cfg.KoraSecretKey,
	}

	// In-app notices (maintenance, news, "please update"). A notice can stop new orders, which the
	// bills API asks about through the gate.
	notices := &notify.Service{Store: noticeStore, Log: log, Events: events}
	api.Gate = notices

	// The admin endpoints every feature shares: tokens, audit, events, who am I, the server's version.
	admin := &platform.Admin{
		Store: shared, Log: log, Alerts: alerts,
		Info: platform.ServerInfo{
			Version: buildVersion(), GoVersion: runtime.Version(), Env: cfg.Env, PaymentProvider: cfg.PaymentProvider,
			ABSSandbox: cfg.ABSSandbox, DirectTestRoutes: !cfg.RealMoney(), StartedAt: time.Now(),
		},
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
		Handler:           routes(cfg, log, api, admin, notices, directRoutes),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // the slowest call out is ABS at 30s
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go api.Sweep(ctx, 5*time.Minute)
	go shared.RunRetention(ctx, time.Duration(cfg.EventRetentionDays)*24*time.Hour, log)
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
		log.Info("Server", "detail", fmt.Sprintf("listening on %s · env=%s · abs_sandbox=%t · version %s", cfg.HTTPAddr, cfg.Env, cfg.ABSSandbox, buildVersion()))
		events.Record(platform.LevelInfo, "server.started", "", fmt.Sprintf("started: version %s, %s", buildVersion(), cfg.Env), nil)
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
	events.Record(platform.LevelInfo, "server.stopped", "", "stopped cleanly", nil)
	events.Close() // write what is queued, before the database closes
}
