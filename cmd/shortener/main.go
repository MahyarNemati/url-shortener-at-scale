// Command shortener serves the URL shortener.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MahyarNemati/url-shortener-at-scale/internal/analytics"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/cache"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/httpapi"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/shortid"
	"github.com/MahyarNemati/url-shortener-at-scale/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr        = flag.String("addr", envOr("SHORTENER_ADDR", ":8080"), "listen address")
		baseURL     = flag.String("base-url", envOr("SHORTENER_BASE_URL", "http://localhost:8080"), "public origin for generated short URLs")
		instance    = flag.Int("instance", envInt("SHORTENER_INSTANCE", 0), "instance id for ID generation (0-1023), unique per process")
		cacheTTL    = flag.Duration("cache-ttl", envDuration("SHORTENER_CACHE_TTL", 5*time.Minute), "how long to cache a resolved link")
		negTTL      = flag.Duration("cache-negative-ttl", envDuration("SHORTENER_CACHE_NEGATIVE_TTL", 10*time.Second), "how long to cache a miss")
		cacheSize   = flag.Int("cache-size", envInt("SHORTENER_CACHE_SIZE", 100000), "maximum cached entries")
		clickhouse  = flag.String("clickhouse", os.Getenv("SHORTENER_CLICKHOUSE"), "ClickHouse HTTP endpoint; empty disables click analytics")
		chDatabase  = flag.String("clickhouse-db", envOr("SHORTENER_CLICKHOUSE_DB", "default"), "ClickHouse database")
		migrate     = flag.Bool("migrate", false, "apply the ClickHouse schema and exit")
		permanent   = flag.Bool("permanent", envBool("SHORTENER_PERMANENT_REDIRECT", false), "use 301 instead of 302")
		trustProxy  = flag.Bool("trust-proxy", envBool("SHORTENER_TRUST_PROXY", false), "believe X-Forwarded-For (only behind a proxy you control)")
		logFormat   = flag.String("log-format", envOr("SHORTENER_LOG_FORMAT", "text"), "text or json")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("shortener", version)
		return nil
	}

	log, err := newLogger(*logFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(log)

	token := os.Getenv("SHORTENER_TOKEN")
	if token == "" {
		// Refusing is the safe reading: without a token anyone can mint links
		// pointing anywhere, which is a phishing service.
		return errors.New("set SHORTENER_TOKEN; refusing to run with an unauthenticated management API")
	}
	if len(token) < 16 {
		return errors.New("SHORTENER_TOKEN must be at least 16 characters")
	}

	// Analytics first, so -migrate can exit before anything binds a port.
	var (
		sink     analytics.Sink
		chSink   *analytics.ClickHouse
		recorder *analytics.Recorder
	)
	if *clickhouse != "" {
		chSink, err = analytics.NewClickHouse(analytics.ClickHouseOptions{
			Endpoint: *clickhouse,
			Database: *chDatabase,
			User:     os.Getenv("SHORTENER_CLICKHOUSE_USER"),
			Password: os.Getenv("SHORTENER_CLICKHOUSE_PASSWORD"),
		})
		if err != nil {
			return err
		}
		if *migrate {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := chSink.Migrate(ctx); err != nil {
				return err
			}
			fmt.Println("clickhouse schema applied")
			return nil
		}
		sink = chSink
	} else {
		if *migrate {
			return errors.New("-migrate needs -clickhouse")
		}
		log.Warn("no ClickHouse endpoint; click analytics are discarded")
		sink = &analytics.Discard{}
	}
	recorder = analytics.NewRecorder(sink, analytics.Options{Logger: log})
	defer recorder.Close()

	gen, err := shortid.NewGenerator(*instance)
	if err != nil {
		return err
	}

	st := store.NewMemory()
	defer st.Close()

	c := cache.New(st, cache.Options{
		TTL: *cacheTTL, NegativeTTL: *negTTL, Capacity: *cacheSize,
	})

	redirectStatus := http.StatusFound
	if *permanent {
		redirectStatus = http.StatusMovedPermanently
	}

	srv, err := httpapi.New(httpapi.Deps{
		Store:          st,
		Cache:          c,
		Generator:      gen,
		Obfuscator:     shortid.NewObfuscator(os.Getenv("SHORTENER_SLUG_SECRET")),
		Analytics:      recorder,
		Log:            log,
		BaseURL:        *baseURL,
		Token:          token,
		TrustProxy:     *trustProxy,
		RedirectStatus: redirectStatus,
	})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// Tight timeouts: every handler is either a cache lookup or a small
		// write, so anything slow is a stuck client rather than real work.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening",
			"addr", *addr, "base_url", *baseURL, "instance", *instance,
			"version", version, "redirect", redirectStatus,
			"clickhouse", *clickhouse != "",
			"slug_obfuscation", os.Getenv("SHORTENER_SLUG_SECRET") != "")
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "err", err)
	}
	// Close the recorder after the server, so clicks served during shutdown
	// are still flushed.
	stats := recorder.Stats()
	log.Info("click analytics", "enqueued", stats.Enqueued, "written", stats.Written,
		"dropped", stats.Dropped, "failed", stats.Failed)
	log.Info("stopped")
	return nil
}

func newLogger(format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	switch strings.ToLower(format) {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stdout, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stdout, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
