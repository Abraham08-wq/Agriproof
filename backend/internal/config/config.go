// Package config loads settings from environment variables.
package config

import (
	"errors"
	"os"
	"strconv"
)

type Config struct {
	Port        string
	APIKey      string
	DatabaseURL string
	AIURL       string

	ChainRPCURL     string
	ChainID         int64
	ChainContract   string
	ChainPrivateKey string

	MpesaEnv            string
	MpesaConsumerKey    string
	MpesaConsumerSecret string
	MpesaShortcode      string
	MpesaPasskey        string
	MpesaCallbackURL    string

	// Payments: test (simulator), intasend, mpesa, or auto (picks by which keys are set)
	PaymentsProvider         string
	IntaSendSecretKey        string
	IntaSendPublishableKey   string
	IntaSendEnv              string // sandbox | live
	IntaSendWalletID         string
	IntaSendWebhookChallenge string
	PayoutMax                int    // largest single payment to one farmer
	MonitorAPIKey            string // read-only key for the watchman
	InboundSecret            string // secret part of the incoming-SMS callback URL
	PublicURL                string // e.g. https://agriproof.example.com, used in printed QR codes
	ReconTolerance           int    // allowed difference in the daily money check
	PayoutFeeReserve         int    // money held back per payout for the payment company's fee

	SessionSecret string // keys one-time code hashes; at least 32 characters
	TrustProxy    bool   // read client IPs from X-Forwarded-For (only behind your own reverse proxy)

	SMSDailyLimit int // safety cap on text messages per day

	SMSEnv      string
	SMSUsername string
	SMSAPIKey   string
	SMSSenderID string
}

func Load() (Config, error) {
	c := Config{
		Port:                     get("PORT", "8080"),
		APIKey:                   os.Getenv("API_KEY"),
		DatabaseURL:              os.Getenv("DATABASE_URL"),
		AIURL:                    get("AI_SERVICE_URL", "http://localhost:8000"),
		ChainRPCURL:              os.Getenv("CHAIN_RPC_URL"),
		ChainContract:            os.Getenv("CHAIN_CONTRACT_ADDRESS"),
		ChainPrivateKey:          os.Getenv("CHAIN_PRIVATE_KEY"),
		MpesaEnv:                 get("MPESA_ENV", "sandbox"),
		MpesaConsumerKey:         os.Getenv("MPESA_CONSUMER_KEY"),
		MpesaConsumerSecret:      os.Getenv("MPESA_CONSUMER_SECRET"),
		MpesaShortcode:           get("MPESA_SHORTCODE", "174379"),
		MpesaPasskey:             os.Getenv("MPESA_PASSKEY"),
		MpesaCallbackURL:         os.Getenv("MPESA_CALLBACK_URL"),
		SessionSecret:            os.Getenv("SESSION_SECRET"),
		PaymentsProvider:         get("PAYMENTS_PROVIDER", "auto"),
		IntaSendSecretKey:        os.Getenv("INTASEND_SECRET_KEY"),
		IntaSendPublishableKey:   os.Getenv("INTASEND_PUBLISHABLE_KEY"),
		IntaSendEnv:              get("INTASEND_ENV", "sandbox"),
		IntaSendWalletID:         os.Getenv("INTASEND_WALLET_ID"),
		IntaSendWebhookChallenge: os.Getenv("INTASEND_WEBHOOK_CHALLENGE"),
		PayoutMax:                getInt("PAYOUT_MAX", 300000),
		MonitorAPIKey:            os.Getenv("MONITOR_API_KEY"),
		InboundSecret:            os.Getenv("INBOUND_SECRET"),
		PublicURL:                os.Getenv("PUBLIC_URL"),
		ReconTolerance:           getInt("RECON_TOLERANCE", 0),
		PayoutFeeReserve:         getInt("PAYOUT_FEE_RESERVE", 120),
		TrustProxy:               os.Getenv("TRUST_PROXY") == "true",
		SMSDailyLimit:            getInt("SMS_DAILY_LIMIT", 500),
		SMSEnv:                   get("AT_ENV", "sandbox"),
		SMSUsername:              os.Getenv("AT_USERNAME"),
		SMSAPIKey:                os.Getenv("AT_API_KEY"),
		SMSSenderID:              os.Getenv("AT_SENDER_ID"),
	}
	id, err := strconv.ParseInt(get("CHAIN_ID", "31337"), 10, 64)
	if err != nil {
		return c, errors.New("CHAIN_ID must be a number")
	}
	c.ChainID = id

	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if len(c.APIKey) < 16 || c.APIKey == "change-me-to-a-long-random-string" {
		return c, errors.New("API_KEY must be set to a random string of at least 16 characters")
	}
	if len(c.SessionSecret) < 32 {
		return c, errors.New("SESSION_SECRET must be a random string of at least 32 characters (run: make setup)")
	}
	return c, nil
}

func (c Config) AnchoringEnabled() bool {
	return c.ChainRPCURL != "" && c.ChainContract != "" && c.ChainPrivateKey != ""
}

func get(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}
