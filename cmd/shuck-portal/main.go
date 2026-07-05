// Command shuck-portal is the self-service token portal of shuck's opt-in
// self-hosted mode (JUS-90): a small private web app where a user optionally
// passes a generic OIDC gate, proves control of a GitHub account via the
// GitHub App's user-authorization flow, is validated against the
// installation (org membership, or account ownership for personal
// installs), and receives a Shuck token shown exactly once. Only the
// token's SHA-256 lands in the gateway token table; regenerate atomically
// revokes the old token. A daily sweep re-validates every token's user
// against current org membership and revokes departed members.
//
// It runs as a plain HTTP server, or — auto-detected — as a Lambda behind a
// function URL (the JUS-92 serverless deployment; function URLs terminate
// TLS, which the Secure session cookies require). `shuck-portal sweep`
// instead runs one sweep pass and exits, for a CronJob; in Lambda mode
// SHUCK_PORTAL_ROLE=sweep does the same per invocation for an EventBridge
// schedule.
//
// Configuration is environment-only (deploy tooling injects secrets;
// JUS-92/93 own that wiring):
//
//	SHUCK_TOKEN_TABLE           gateway token table name (required)
//	SHUCK_BASE_URL              external portal origin for OAuth callbacks,
//	                            e.g. https://shuck.corp.example (required)
//	SHUCK_SESSION_SECRET        session HMAC key, >= 32 bytes (required in
//	                            serve mode)
//	SHUCK_GITHUB_CLIENT_ID      GitHub App OAuth client id (required in
//	                            serve mode)
//	SHUCK_GITHUB_CLIENT_SECRET  GitHub App OAuth client secret (required in
//	                            serve mode)
//
// Exactly one validation mode:
//
//	SHUCK_GITHUB_ORG            org-install mode: org login to check
//	                            membership of. Also requires
//	                            SHUCK_GITHUB_APP_ID,
//	                            SHUCK_GITHUB_APP_PRIVATE_KEY (or _FILE), and
//	                            SHUCK_GITHUB_INSTALLATION_ID for the
//	                            members:read installation token.
//	SHUCK_GITHUB_ACCOUNT_ID     personal-install mode: the account's numeric
//	                            user ID; only that user may hold a token.
//
// Optional:
//
//	SHUCK_OIDC_ISSUER           OIDC issuer URL; enables the SSO gate
//	SHUCK_OIDC_CLIENT_ID        OIDC client id (required with issuer)
//	SHUCK_OIDC_CLIENT_SECRET    OIDC client secret (required with issuer)
//	SHUCK_GITHUB_URL            GitHub web origin (GHES; default
//	                            https://github.com)
//	SHUCK_GITHUB_API_URL        GitHub API base (GHES)
//	SHUCK_SWEEP_INTERVAL        re-validation interval (default 24h;
//	                            server mode — Lambda mode schedules the
//	                            sweep role externally)
//	SHUCK_SESSION_TTL           session lifetime (default 1h)
//	SHUCK_ADDR                  listen address (default :8080; server mode)
//	SHUCK_PORTAL_ROLE           Lambda mode only: unset serves the portal;
//	                            "sweep" runs one re-validation pass per
//	                            invocation (EventBridge schedule)
//
// This binary is part of the self-hosted backend only. The portable shuck
// CLI does not link it and is unaffected by it (see docs/V2.md).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/justanotherspy/shuck/internal/gh"
	"github.com/justanotherspy/shuck/internal/lambdahttp"
	"github.com/justanotherspy/shuck/internal/portal"
	"github.com/justanotherspy/shuck/internal/portal/awsx"
	"github.com/justanotherspy/shuck/internal/worker"
)

// version is stamped at build time via -X main.version (Makefile /
// Dockerfile.backend); untagged builds report "dev".
var version = "dev"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	log.Info("shuck-portal starting", "version", version)
	if err := run(context.Background(), log); err != nil {
		log.Error("shuck-portal failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	table := os.Getenv("SHUCK_TOKEN_TABLE")
	if table == "" {
		return fmt.Errorf("SHUCK_TOKEN_TABLE is required")
	}
	validator, err := buildValidator()
	if err != nil {
		return err
	}
	sweepInterval, err := durationEnv("SHUCK_SWEEP_INTERVAL", portal.DefaultSweepInterval)
	if err != nil {
		return err
	}

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	store := awsx.NewDynamoTokenStore(dynamodb.NewFromConfig(awsCfg), table)
	sweeper := &portal.Sweeper{Store: store, Validate: validator, Interval: sweepInterval, Log: log}

	// One-shot sweep mode for cron scheduling (JUS-92/93).
	if len(os.Args) > 1 && os.Args[1] == "sweep" {
		revoked := sweeper.Sweep(ctx)
		log.Info("sweep pass finished", "revoked", revoked)
		return nil
	}

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		if os.Getenv("SHUCK_PORTAL_ROLE") == "sweep" {
			log.Info("starting in Lambda mode", "role", "sweep")
			lambda.StartWithOptions(func(ctx context.Context) error {
				revoked := sweeper.Sweep(ctx)
				log.Info("sweep pass finished", "revoked", revoked)
				return nil
			}, lambda.WithContext(ctx))
			return nil
		}
		handler, err := buildHandler(ctx, store, validator, log)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		handler.Register(mux)
		// No in-process sweeper here: Lambda freezes between invocations,
		// so re-validation runs as the separately scheduled sweep role.
		log.Info("starting in Lambda mode", "role", "serve", "oidc", handler.OIDC != nil)
		lambda.StartWithOptions(lambdahttp.FunctionURLHandler(mux), lambda.WithContext(ctx))
		return nil
	}

	handler, err := buildHandler(ctx, store, validator, log)
	if err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go sweeper.Run(runCtx)

	mux := http.NewServeMux()
	handler.Register(mux)
	addr := os.Getenv("SHUCK_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	log.Info("portal listening", "addr", addr, "oidc", handler.OIDC != nil)

	select {
	case err := <-errCh:
		return err
	case <-runCtx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

// buildHandler wires the serve-mode Handler from the env.
func buildHandler(ctx context.Context, store portal.TokenStore, validator portal.Validator, log *slog.Logger) (*portal.Handler, error) {
	baseURL := strings.TrimSuffix(os.Getenv("SHUCK_BASE_URL"), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("SHUCK_BASE_URL is required")
	}
	secret := os.Getenv("SHUCK_SESSION_SECRET")
	if len(secret) < portal.MinSessionSecret {
		return nil, fmt.Errorf("SHUCK_SESSION_SECRET must be at least %d bytes", portal.MinSessionSecret)
	}
	clientID := os.Getenv("SHUCK_GITHUB_CLIENT_ID")
	clientSecret := os.Getenv("SHUCK_GITHUB_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("SHUCK_GITHUB_CLIENT_ID and SHUCK_GITHUB_CLIENT_SECRET are required")
	}
	sessionTTL, err := durationEnv("SHUCK_SESSION_TTL", 0)
	if err != nil {
		return nil, err
	}

	h := &portal.Handler{
		Store:    store,
		Validate: validator,
		GitHub: &portal.GitHubOAuth{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			WebBase:      os.Getenv("SHUCK_GITHUB_URL"),
			APIBase:      os.Getenv("SHUCK_GITHUB_API_URL"),
		},
		Sessions: &portal.SessionCodec{Secret: []byte(secret), TTL: sessionTTL},
		BaseURL:  baseURL,
		Log:      log,
	}

	issuer := os.Getenv("SHUCK_OIDC_ISSUER")
	oidcID := os.Getenv("SHUCK_OIDC_CLIENT_ID")
	oidcSecret := os.Getenv("SHUCK_OIDC_CLIENT_SECRET")
	switch {
	case issuer == "" && oidcID == "" && oidcSecret == "":
		// OIDC disabled: GitHub authentication alone gates the UI.
	case issuer != "" && oidcID != "" && oidcSecret != "":
		oidc, err := portal.NewOIDC(ctx, issuer, oidcID, oidcSecret)
		if err != nil {
			return nil, err
		}
		h.OIDC = oidc
	default:
		return nil, fmt.Errorf("SHUCK_OIDC_ISSUER, SHUCK_OIDC_CLIENT_ID, and SHUCK_OIDC_CLIENT_SECRET must be set together")
	}
	return h, nil
}

// buildValidator picks the validation mode from the mutually exclusive env
// pair.
func buildValidator() (portal.Validator, error) {
	org := os.Getenv("SHUCK_GITHUB_ORG")
	account := os.Getenv("SHUCK_GITHUB_ACCOUNT_ID")
	switch {
	case org != "" && account != "":
		return nil, fmt.Errorf("SHUCK_GITHUB_ORG and SHUCK_GITHUB_ACCOUNT_ID are mutually exclusive")
	case account != "":
		id, err := strconv.ParseInt(account, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse SHUCK_GITHUB_ACCOUNT_ID: %w", err)
		}
		return &portal.AccountValidator{AccountID: id}, nil
	case org != "":
		appID, err := parseAppID(os.Getenv("SHUCK_GITHUB_APP_ID"))
		if err != nil {
			return nil, err
		}
		keyPEM, err := loadPrivateKey()
		if err != nil {
			return nil, err
		}
		installation := os.Getenv("SHUCK_GITHUB_INSTALLATION_ID")
		installationID, err := strconv.ParseInt(installation, 10, 64)
		if err != nil || installationID <= 0 {
			return nil, fmt.Errorf("SHUCK_GITHUB_INSTALLATION_ID is required in org mode")
		}
		tokens, err := worker.NewAppTokenSource(appID, keyPEM)
		if err != nil {
			return nil, err
		}
		apiBase := os.Getenv("SHUCK_GITHUB_API_URL")
		if apiBase != "" {
			tokens.BaseURL = apiBase
		}
		return &portal.OrgValidator{
			Org:            org,
			InstallationID: installationID,
			Tokens:         tokens,
			NewClient: func(token string) (portal.OrgAPI, error) {
				return gh.NewEnterprise(token, apiBase)
			},
		}, nil
	default:
		return nil, fmt.Errorf("one of SHUCK_GITHUB_ORG or SHUCK_GITHUB_ACCOUNT_ID is required")
	}
}

// parseAppID validates the required numeric App ID.
func parseAppID(v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("SHUCK_GITHUB_APP_ID is required in org mode")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse SHUCK_GITHUB_APP_ID: %w", err)
	}
	return id, nil
}

// loadPrivateKey reads the App private key from the env var or the mounted
// file, preferring the inline value when both are set.
func loadPrivateKey() ([]byte, error) {
	if v := os.Getenv("SHUCK_GITHUB_APP_PRIVATE_KEY"); v != "" {
		return []byte(v), nil
	}
	if path := os.Getenv("SHUCK_GITHUB_APP_PRIVATE_KEY_FILE"); path != "" {
		key, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("read SHUCK_GITHUB_APP_PRIVATE_KEY_FILE: %w", err)
		}
		return key, nil
	}
	return nil, fmt.Errorf("SHUCK_GITHUB_APP_PRIVATE_KEY or SHUCK_GITHUB_APP_PRIVATE_KEY_FILE is required in org mode")
}

// durationEnv parses an optional duration env var.
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return d, nil
}
