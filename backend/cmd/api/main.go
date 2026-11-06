// Command api runs the AgriProof backend.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agriproof/backend/internal/api"
	"github.com/agriproof/backend/internal/auth"
	"github.com/agriproof/backend/internal/chain"
	"github.com/agriproof/backend/internal/config"
	"github.com/agriproof/backend/internal/core"
	"github.com/agriproof/backend/internal/mpesa"
	"github.com/agriproof/backend/internal/payments"
	"github.com/agriproof/backend/internal/risk"
	"github.com/agriproof/backend/internal/sms"
	"github.com/agriproof/backend/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "create-admin" {
		if err := createAdmin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "payments-check" {
		if err := paymentsCheck(); err != nil {
			fmt.Fprintln(os.Stderr, "Problem:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	log.Info("database ready")

	var anchorer *chain.Anchorer
	if cfg.AnchoringEnabled() {
		anchorer, err = chain.NewAnchorer(cfg.ChainRPCURL, cfg.ChainID, cfg.ChainContract, cfg.ChainPrivateKey)
		if err != nil {
			return err
		}
		log.Info("blockchain anchoring enabled", "relayer", anchorer.From(), "contract", cfg.ChainContract)
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := anchorer.CheckContract(checkCtx); err != nil {
			log.Error("blockchain check failed; anchoring will be skipped until fixed", "err", err)
		}
		cancel()
	} else {
		log.Warn("blockchain anchoring disabled (set CHAIN_CONTRACT_ADDRESS to enable)")
	}

	mp := mpesa.New(mpesa.Config{
		Env: cfg.MpesaEnv, ConsumerKey: cfg.MpesaConsumerKey, ConsumerSecret: cfg.MpesaConsumerSecret,
		Shortcode: cfg.MpesaShortcode, Passkey: cfg.MpesaPasskey, CallbackURL: cfg.MpesaCallbackURL,
	})
	pay, err := pickProvider(cfg, mp)
	if err != nil {
		return err
	}
	if pay.Live() {
		log.Warn("LIVE PAYMENTS: real money moves", "provider", pay.Name())
	} else {
		log.Warn("payments in test mode: no real money moves", "provider", pay.Name())
	}

	sender := sms.New(sms.Config{Env: cfg.SMSEnv, Username: cfg.SMSUsername, APIKey: cfg.SMSAPIKey, SenderID: cfg.SMSSenderID}, log)
	if sender.TestMode() {
		log.Warn("SMS in test mode: login codes are written to this log (add Africa's Talking keys to .env to send real SMS)")
	}

	sender.SetDailyLimit(cfg.SMSDailyLimit)
	svc := core.NewService(db, risk.NewClient(cfg.AIURL), pay, anchorer, sender, []byte(cfg.SessionSecret), log)
	svc.SetPaymentLimits(int64(cfg.PayoutMax), int64(cfg.ReconTolerance), int64(cfg.PayoutFeeReserve))
	if err := svc.CheckMoneyMode(ctx); err != nil {
		return err
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	go svc.RunPaymentWorker(workerCtx)
	go func() {
		for {
			svc.PurgeExpired(context.Background())
			time.Sleep(time.Hour)
		}
	}()
	apiServer := api.NewServer(svc, cfg.APIKey, cfg.TrustProxy, cfg.IntaSendWebhookChallenge, log)
	apiServer.SetKeys(cfg.MonitorAPIKey, cfg.InboundSecret, cfg.PublicURL)
	handler := apiServer.Routes()
	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("AgriProof API listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	svc.Wait()
	return err
}

// createAdmin is run as: create-admin <username>, with the password on standard input.
// It prints the authenticator secret to add to Google Authenticator, Microsoft Authenticator or similar.
func createAdmin(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: create-admin <username>   (password is read from standard input)")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	pw, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	pw = strings.TrimRight(pw, "\r\n")
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db); err != nil {
		return err
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := core.NewService(db, nil, nil, nil, nil, []byte(cfg.SessionSecret), quiet)
	secret, err := svc.CreateAdmin(ctx, args[0], pw)
	if err != nil {
		return err
	}
	username := strings.ToLower(strings.TrimSpace(args[0]))
	fmt.Println("SECRET=" + secret)
	fmt.Println("URI=" + auth.TOTPURI("AgriProof Field Office", username, secret))
	return nil
}

// pickProvider chooses how money moves. "auto" uses IntaSend if its keys are set, then Safaricom
// Daraja if its keys are set, otherwise test mode.
func pickProvider(cfg config.Config, mp *mpesa.Client) (payments.Provider, error) {
	hasIntaSend := cfg.IntaSendSecretKey != "" && cfg.IntaSendPublishableKey != ""
	choice := cfg.PaymentsProvider
	if choice == "auto" || choice == "" {
		switch {
		case hasIntaSend:
			choice = "intasend"
		case !mp.TestMode():
			choice = "mpesa"
		default:
			choice = "test"
		}
	}
	switch choice {
	case "test":
		return payments.Simulator{}, nil
	case "intasend":
		if !hasIntaSend {
			return nil, errors.New("PAYMENTS_PROVIDER=intasend needs INTASEND_SECRET_KEY and INTASEND_PUBLISHABLE_KEY")
		}
		if cfg.IntaSendEnv != "sandbox" && cfg.IntaSendEnv != "live" {
			return nil, errors.New("INTASEND_ENV must be sandbox or live")
		}
		return payments.NewIntaSend(cfg.IntaSendSecretKey, cfg.IntaSendPublishableKey, cfg.IntaSendEnv, cfg.IntaSendWalletID), nil
	case "mpesa":
		if mp.TestMode() {
			return nil, errors.New("PAYMENTS_PROVIDER=mpesa needs the MPESA_* keys")
		}
		return payments.Daraja{C: mp}, nil
	}
	return nil, errors.New("PAYMENTS_PROVIDER must be auto, test, intasend or mpesa")
}

// paymentsCheck confirms the payment keys work by reading the account balance. No money moves.
func paymentsCheck() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	mp := mpesa.New(mpesa.Config{Env: cfg.MpesaEnv, ConsumerKey: cfg.MpesaConsumerKey, ConsumerSecret: cfg.MpesaConsumerSecret,
		Shortcode: cfg.MpesaShortcode, Passkey: cfg.MpesaPasskey, CallbackURL: cfg.MpesaCallbackURL})
	pay, err := pickProvider(cfg, mp)
	if err != nil {
		return err
	}
	mode := "TEST (no real money)"
	if pay.Live() {
		mode = "LIVE (real money)"
	} else if pay.Name() != "test" {
		mode = "SANDBOX (test money)"
	}
	fmt.Printf("Payment provider: %s, %s\n", pay.Name(), mode)
	if pay.Name() == "test" {
		fmt.Println("No payment keys set. Add INTASEND_SECRET_KEY and INTASEND_PUBLISHABLE_KEY to .env to connect IntaSend.")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	bal, ok, err := pay.Balance(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("the payment company refused or did not answer: %v", err)
	case !ok:
		fmt.Println("Keys look set. This provider does not report a balance, so the daily money check will show 'unavailable'.")
	default:
		fmt.Printf("Connected. Account balance: KES %d\n", bal)
	}
	if pay.Name() == "intasend" && cfg.IntaSendWebhookChallenge == "" {
		fmt.Println("Note: INTASEND_WEBHOOK_CHALLENGE is empty, so IntaSend notifications are refused; payments are still confirmed by checking every 15-20 seconds.")
	}
	return nil
}
