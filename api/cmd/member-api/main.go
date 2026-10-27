// Command member-api is the backend of member.vettid.org: sessions,
// roster, proposals, the beta program and comments. It integrates with
// the VettID service vault through its SDK and webhooks, exactly as a
// third-party application would (MEMBER-SERVICE-PLAN §3, §7.3).
//
//	member-api serve     serve the API (configuration from the environment)
//	member-api version   print the version
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/vettid/member-service/api/internal/mail"
	"github.com/vettid/member-service/api/internal/session"
	"github.com/vettid/member-service/api/internal/store"
	"github.com/vettid/service-vault/sdk/go/servicevault"
	"github.com/vettid/service-vault/sdk/go/webhook"
)

// version is set at build time with -ldflags '-X main.version=...'.
var version = "dev"

func main() {
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: member-api serve | version\n")
	}
	flag.Parse()
	switch flag.Arg(0) {
	case "version":
		fmt.Println(version)
	case "serve":
		log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		if err := serve(log, os.Getenv); err != nil {
			log.Error("exit", "err", err.Error())
			os.Exit(1)
		}
	default:
		flag.Usage()
		os.Exit(2)
	}
}

// cfg is the configuration (README "Configuration").
type cfg struct {
	Addr         string // MEMBER_API_ADDR
	VaultURL     string // SERVICE_VAULT_URL
	VaultKey     string // SERVICE_VAULT_API_KEY
	VaultPin     string // SERVICE_VAULT_PIN
	WebhookKey   []byte // WEBHOOK_SECRET
	Pepper       []byte // MEMBER_API_PEPPER
	Table        string // DYNAMODB_TABLE
	DynamoURL    string // DYNAMODB_ENDPOINT (local development)
	MailFrom     string // MAIL_FROM: SES
	MailDir      string // MAIL_DIR: local development, instead of SES
	ClientIPFrom string // CLIENT_IP_HEADER
	OriginSecret []byte // ORIGIN_VERIFY_SECRET
}

// OriginHeader carries the secret the edge (CloudFront) adds to every
// request it forwards (vettid.org StaticSite apiOriginHeaders).
const OriginHeader = "X-Origin-Verify"

func loadConfig(getenv func(string) string) (*cfg, error) {
	c := &cfg{Addr: getenv("MEMBER_API_ADDR"), VaultURL: getenv("SERVICE_VAULT_URL"), VaultKey: getenv("SERVICE_VAULT_API_KEY"),
		VaultPin: getenv("SERVICE_VAULT_PIN"), WebhookKey: []byte(getenv("WEBHOOK_SECRET")), Pepper: []byte(getenv("MEMBER_API_PEPPER")),
		Table: getenv("DYNAMODB_TABLE"), DynamoURL: getenv("DYNAMODB_ENDPOINT"), MailFrom: getenv("MAIL_FROM"),
		MailDir: getenv("MAIL_DIR"), ClientIPFrom: getenv("CLIENT_IP_HEADER"), OriginSecret: []byte(getenv("ORIGIN_VERIFY_SECRET"))}
	var missing []string
	for name, v := range map[string]string{"MEMBER_API_ADDR": c.Addr, "SERVICE_VAULT_URL": c.VaultURL, "SERVICE_VAULT_API_KEY": c.VaultKey,
		"SERVICE_VAULT_PIN": c.VaultPin, "DYNAMODB_TABLE": c.Table} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	if len(c.WebhookKey) < webhook.MinSecretLen || len(c.Pepper) < 32 {
		return nil, errors.New("WEBHOOK_SECRET and MEMBER_API_PEPPER must be at least 32 bytes")
	}
	if len(c.OriginSecret) > 0 && len(c.OriginSecret) < 32 {
		return nil, errors.New("ORIGIN_VERIFY_SECRET must be at least 32 bytes")
	}
	if c.ClientIPFrom != "" && len(c.OriginSecret) == 0 {
		// The header is trusted only from the edge.
		return nil, errors.New("CLIENT_IP_HEADER needs ORIGIN_VERIFY_SECRET")
	}
	if (c.MailFrom == "") == (c.MailDir == "") {
		return nil, errors.New("set exactly one of MAIL_FROM (SES) and MAIL_DIR (local development)")
	}
	return c, nil
}

// fromEdge refuses requests without the edge's secret header (403), so
// the origin cannot be reached around CloudFront and the client-address
// header cannot be forged. /api/health is exempt (the load balancer's
// and ECS's checks come straight to the task).
func fromEdge(secret []byte, next http.Handler) http.Handler {
	if len(secret) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" && subtle.ConstantTimeCompare([]byte(r.Header.Get(OriginHeader)), secret) != 1 {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP reads the visitor's address from a header the edge sets and
// the visitor cannot (CloudFront-Viewer-Address: "address:port"); ""
// (the server then uses the remote address) when it is missing.
func clientIP(header string) func(*http.Request) string {
	if header == "" {
		return nil
	}
	return func(r *http.Request) string {
		v := r.Header.Get(header)
		if i := strings.LastIndexByte(v, ':'); i > 0 {
			v = v[:i] // the port
		}
		return v
	}
}

func serve(log *slog.Logger, getenv func(string) string) error {
	c, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	ddb := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if c.DynamoURL != "" {
			o.BaseEndpoint = aws.String(c.DynamoURL)
		}
	})
	var mailer mail.Mailer = &mail.Dir{Path: c.MailDir}
	if c.MailFrom != "" {
		mailer = &mail.SES{Client: sesv2.NewFromConfig(awsCfg), From: c.MailFrom}
	}
	vault, err := servicevault.New(servicevault.Config{BaseURL: c.VaultURL, APIKey: c.VaultKey, Pin: c.VaultPin})
	if err != nil {
		return fmt.Errorf("service vault client: %w", err)
	}
	sessions := &session.Server{Store: &store.Dynamo{Client: ddb, Table: c.Table}, Vault: vault, Mail: mailer, Pepper: c.Pepper,
		Webhook: &servicevault.Receiver{Secret: c.WebhookKey}, ClientIP: clientIP(c.ClientIPFrom), Log: log}
	mux := http.NewServeMux()
	mux.Handle("/api/", sessions.Handler())
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: c.Addr, Handler: fromEdge(c.OriginSecret, mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("serving", "version", version, "addr", c.Addr)
	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
