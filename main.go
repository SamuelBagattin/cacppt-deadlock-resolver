package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var version = "dev"

// Config holds all controller configuration.
type Config struct {
	PollInterval   time.Duration
	StuckThreshold time.Duration
	DryRun         bool
	HealthPort     int
	TCPAPIVersion  string
	Kubeconfig     string
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cacppt-deadlock-resolver",
		Short: "Automated workaround for CACPPT rolling update etcd deadlock",
		Long: `Watches all TalosControlPlane resources across namespaces and automatically
resolves the etcd healthcheck deadlock caused by the non-atomic
gracefulEtcdLeave / Machine delete in CACPPT.

Detection:
  EtcdClusterHealthy=False + machines > desired replicas for > threshold

Resolution:
  Cross-reference etcd member list (via Talos gRPC API) with Machine list,
  find Machine not in etcd, and delete it to unblock the rollout.

All flags can also be set via environment variables (flag takes precedence).`,
		Version:      version,
		SilenceUsage: true,
		RunE:         run,
	}

	f := cmd.Flags()
	f.String("poll-interval", "20s", "Polling interval for TalosControlPlane resources (env: POLL_INTERVAL)")
	f.String("stuck-threshold", "120s", "Time before a deadlock is confirmed (env: STUCK_THRESHOLD)")
	f.Bool("dry-run", false, "Log actions without executing them (env: DRY_RUN)")
	f.Int("health-port", 8080, "Port for health check endpoints (env: HEALTH_PORT)")
	f.String("tcp-api-version", "v1alpha3", "TalosControlPlane API version (env: TCP_API_VERSION)")
	f.String("kubeconfig", "", "Path to kubeconfig file for out-of-cluster usage (env: KUBECONFIG)")
	f.String("log-level", "info", "Log level: debug, info, warn, error (env: LOG_LEVEL)")
	f.String("log-format", "json", "Log format: json, text (env: LOG_FORMAT)")

	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		bindEnvVars(cmd.Flags())
		return nil
	}

	return cmd
}

func run(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()

	pollInterval, err := parseDuration(mustGetString(f, "poll-interval"))
	if err != nil {
		return fmt.Errorf("invalid --poll-interval: %w", err)
	}
	stuckThreshold, err := parseDuration(mustGetString(f, "stuck-threshold"))
	if err != nil {
		return fmt.Errorf("invalid --stuck-threshold: %w", err)
	}

	cfg := Config{
		PollInterval:   pollInterval,
		StuckThreshold: stuckThreshold,
		DryRun:         mustGetBool(f, "dry-run"),
		HealthPort:     mustGetInt(f, "health-port"),
		TCPAPIVersion:  mustGetString(f, "tcp-api-version"),
		Kubeconfig:     mustGetString(f, "kubeconfig"),
	}

	logger := newLogger(mustGetString(f, "log-level"), mustGetString(f, "log-format"))

	ctrl, err := NewController(cfg, logger)
	if err != nil {
		return fmt.Errorf("creating controller: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go startHealthServer(cfg.HealthPort, logger)

	logger.Info("starting cacppt-deadlock-resolver controller",
		"version", version,
		"pollInterval", cfg.PollInterval,
		"stuckThreshold", cfg.StuckThreshold,
		"dryRun", cfg.DryRun,
		"tcpAPIVersion", cfg.TCPAPIVersion,
	)

	ctrl.Run(ctx)
	logger.Info("controller stopped")
	return nil
}

// bindEnvVars sets flag values from matching environment variables.
// Flags take precedence: env is only applied if the flag wasn't explicitly set.
func bindEnvVars(flags *pflag.FlagSet) {
	flags.VisitAll(func(f *pflag.Flag) {
		envVar := flagToEnv(f.Name)
		if val := os.Getenv(envVar); val != "" && !f.Changed {
			_ = flags.Set(f.Name, val)
		}
	})
}

func flagToEnv(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// parseDuration parses a duration string. Supports Go durations (20s, 2m)
// and plain integers as seconds (20 -> 20s) for backward compatibility.
func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	if secs, err := strconv.Atoi(s); err == nil {
		return time.Duration(secs) * time.Second, nil
	}
	return 0, fmt.Errorf("%q is not a valid duration (use e.g. 20s, 2m, or plain seconds)", s)
}

func newLogger(level, format string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	switch strings.ToLower(format) {
	case "text":
		handler = slog.NewTextHandler(os.Stdout, opts)
	default:
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	return slog.New(handler)
}

func startHealthServer(port int, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	addr := ":" + strconv.Itoa(port)
	logger.Info("health server listening", "addr", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("health server failed", "error", err)
	}
}

func mustGetString(f *pflag.FlagSet, name string) string {
	v, _ := f.GetString(name)
	return v
}

func mustGetBool(f *pflag.FlagSet, name string) bool {
	v, _ := f.GetBool(name)
	return v
}

func mustGetInt(f *pflag.FlagSet, name string) int {
	v, _ := f.GetInt(name)
	return v
}
