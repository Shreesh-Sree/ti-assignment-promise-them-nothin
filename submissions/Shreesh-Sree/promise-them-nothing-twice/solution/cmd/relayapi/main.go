// Command relayapi is the RelayAPI node binary: loads and validates
// policy config (failing to start on a bad one, per DESIGN-NOTES.md),
// picks a coordination strategy from its environment, and serves the
// metered demo endpoint plus the two introspection endpoints.
//
// Every knob here comes from the environment, not flags — this binary is
// meant to run identically inside a container in docker-compose, where
// env vars are the natural place to differ node-1 from node-2 from
// node-3.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"relayapi/internal/coordinator"
	"relayapi/internal/httpapi"
	"relayapi/internal/policy"
	"relayapi/internal/ratelimit"
)

// modeRegistry holds coordinator factories registered by build-tagged
// files (e.g. mode_fixedwindow.go). A normal build leaves this empty,
// so requesting a mode that only exists behind a tag produces a clear
// "rebuild with -tags X" panic rather than silently compiling in broken
// code or failing with an opaque "undefined" error.
type modeFactory func(nodeID string, nodeCount int, clock ratelimit.Clock, logger *slog.Logger) (coordinator.Coordinator, error)

var modeRegistry = map[string]modeFactory{}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	nodeID := envOr("RELAYAPI_NODE_ID", "node-1")
	configPath := envOr("RELAYAPI_CONFIG", "/etc/relayapi/customers.yaml")
	listenAddr := envOr("RELAYAPI_LISTEN_ADDR", ":8080")
	nodeCount := envInt("RELAYAPI_NODE_COUNT", 3)
	mode := envOr("RELAYAPI_COORDINATOR_MODE", "static") // "static" or "peer"

	clock := policy.NewClockFromEnv(logger)

	resolver, err := policy.NewResolver(configPath, clock, logger)
	if err != nil {
		// Fail to start, don't warn — an invalid config must never serve
		// traffic under a silently-wrong limit.
		logger.Error("startup_failed", "component", "policy", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	policy.WatchSIGHUP(ctx, configPath, resolver)

	coord, err := newCoordinator(ctx, mode, nodeID, nodeCount, clock, logger)
	if err != nil {
		logger.Error("startup_failed", "component", "coordinator", "error", err)
		os.Exit(1)
	}

	server := httpapi.NewServer(nodeID, resolver, coord, clock, logger)

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Info("relayapi_starting", "node_id", nodeID, "mode", mode, "node_count", nodeCount, "listen_addr", listenAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server_failed", "error", err)
		os.Exit(1)
	}
}

// newCoordinator constructs the coordination strategy named by mode.
// "static" needs nothing beyond node identity. "peer" additionally reads
// the peer list and proposer identity from the environment and starts its
// background rebalance goroutines against ctx, so they stop cleanly on
// shutdown.
func newCoordinator(ctx context.Context, mode, nodeID string, nodeCount int, clock ratelimit.Clock, logger *slog.Logger) (coordinator.Coordinator, error) {
	switch mode {
	case "static":
		return coordinator.NewStatic(nodeID, nodeCount, clock), nil
	case "peer":
		peers := splitCSV(os.Getenv("RELAYAPI_PEERS")) // e.g. "node-1=http://node1:8080,node-2=http://node2:8080,node-3=http://node3:8080"
		proposer := envOr("RELAYAPI_PROPOSER", "node-1")
		pollInterval := envDuration("RELAYAPI_POLL_INTERVAL", time.Second)
		ackTimeout := envDuration("RELAYAPI_ACK_TIMEOUT", 400*time.Millisecond)
		pc, err := coordinator.NewPeer(coordinator.PeerConfig{
			NodeID:       nodeID,
			NodeCount:    nodeCount,
			Peers:        peers,
			Proposer:     proposer,
			Clock:        clock,
			Logger:       logger,
			PollInterval: pollInterval,
			AckTimeout:   ackTimeout,
		})
		if err != nil {
			return nil, err
		}
		pc.Run(ctx)
		return pc, nil
	default:
		if factory, ok := modeRegistry[mode]; ok {
			return factory(nodeID, nodeCount, clock, logger)
		}
		panic("relayapi: unknown RELAYAPI_COORDINATOR_MODE " + mode +
			" (if you meant 'fixedwindow', rebuild with: go build -tags fixedwindow)")
	}
}

// splitCSV parses "id=url,id=url,..." into a map, skipping empty entries
// so an unset env var yields an empty map rather than one bogus key.
func splitCSV(s string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[0]] = parts[1]
	}
	return out
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
