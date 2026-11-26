// Command webhook runs the NodeRings provider for external-dns as a webhook
// server, usually as a sidecar next to external-dns.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/provider/webhook/api"

	"github.com/noderings/external-dns-noderings-webhook/internal/nrdns"
	"github.com/noderings/external-dns-noderings-webhook/internal/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

type config struct {
	token, tokenFile, org, apiURL string
	domainFilter, excludeDomains  []string
	defaultTTL                    int64
	dryRun                        bool
	webhookAddr, healthAddr       string
	logLevel, logFormat           string
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println(version)
		return
	}
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		os.Exit(2)
	}
	log := newLogger(cfg.logLevel, cfg.logFormat)
	if err := run(cfg, log); err != nil {
		log.Error("webhook stopped", "error", err)
		os.Exit(1)
	}
}

func run(cfg config, log *slog.Logger) error {
	client, err := nrdns.New(nrdns.Config{
		Endpoint:       cfg.apiURL,
		Token:          cfg.token,
		OrganizationID: cfg.org,
		UserAgent:      "external-dns-noderings-webhook/" + version,
		Logger:         log,
	})
	if err != nil {
		return err
	}
	p := provider.New(client, provider.Options{
		DomainFilter: endpoint.NewDomainFilterWithExclusions(cfg.domainFilter, cfg.excludeDomains),
		DefaultTTL:   cfg.defaultTTL,
		DryRun:       cfg.dryRun,
		Logger:       log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Fail fast on a bad token or organization, with a clear message.
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	zones, err := p.Zones(check)
	cancel()
	if err != nil {
		if nrdns.IsUnauthenticated(err) {
			return fmt.Errorf("the NodeRings API rejected the token: %w", err)
		}
		return err
	}
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		names = append(names, nrdns.NormalizeName(z.Name))
	}
	log.Info("starting NodeRings webhook for external-dns", "version", version, "zones", names,
		"domainFilter", cfg.domainFilter, "dryRun", cfg.dryRun, "webhook", cfg.webhookAddr, "health", cfg.healthAddr)

	var ready atomic.Bool
	go serveHealth(ctx, cfg.healthAddr, &ready, log)

	started := make(chan struct{}, 1)
	go api.StartHTTPApi(api.ServerOptions{
		Provider:          p,
		StartedChan:       started,
		ProviderPort:      cfg.webhookAddr,
		ReadTimeout:       time.Minute,
		WriteTimeout:      5 * time.Minute,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxBodySize:       16 << 20,
	})
	select {
	case <-started:
		ready.Store(true)
	case <-ctx.Done():
		return nil
	}
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

func serveHealth(ctx context.Context, addr string, ready *atomic.Bool, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("health server stopped", "error", err)
	}
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		token:          strings.TrimSpace(getenv("NODERINGS_TOKEN")),
		tokenFile:      strings.TrimSpace(getenv("NODERINGS_TOKEN_FILE")),
		org:            strings.TrimSpace(getenv("NODERINGS_ORGANIZATION_ID")),
		apiURL:         strings.TrimSpace(getenv("NODERINGS_API_URL")),
		domainFilter:   list(getenv("DOMAIN_FILTER")),
		excludeDomains: list(getenv("EXCLUDE_DOMAINS")),
		webhookAddr:    or(getenv("WEBHOOK_ADDRESS"), "localhost:8888"),
		healthAddr:     or(getenv("HEALTH_ADDRESS"), ":8080"),
		logLevel:       or(getenv("LOG_LEVEL"), "info"),
		logFormat:      or(getenv("LOG_FORMAT"), "text"),
	}
	if cfg.token == "" && cfg.tokenFile != "" {
		b, err := os.ReadFile(cfg.tokenFile)
		if err != nil {
			return cfg, fmt.Errorf("reading NODERINGS_TOKEN_FILE: %w", err)
		}
		cfg.token = strings.TrimSpace(string(b))
	}
	if cfg.token == "" {
		return cfg, errors.New("set NODERINGS_TOKEN (or NODERINGS_TOKEN_FILE) to a NodeRings service account token")
	}
	if v := getenv("DEFAULT_TTL"); v != "" {
		ttl, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ttl < 1 {
			return cfg, fmt.Errorf("DEFAULT_TTL must be a positive number of seconds, got %q", v)
		}
		cfg.defaultTTL = ttl
	}
	if v := getenv("DRY_RUN"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("DRY_RUN must be true or false, got %q", v)
		}
		cfg.dryRun = b
	}
	return cfg, nil
}

func newLogger(level, format string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: l}
	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func list(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func or(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}
