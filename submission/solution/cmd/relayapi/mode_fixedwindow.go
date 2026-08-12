//go:build fixedwindow

package main

import (
	"log/slog"

	"relayapi/internal/coordinator"
	"relayapi/internal/ratelimit"
)

func init() {
	modeRegistry["fixedwindow"] = func(nodeID string, nodeCount int, clock ratelimit.Clock, logger *slog.Logger) (coordinator.Coordinator, error) {
		logger.Warn("RUNNING DELIBERATELY BROKEN FIXED-WINDOW LIMITER — this mode exists solely to prove the harness catches the boundary bug. Never deploy this.")
		return coordinator.NewFixedWindowStatic(nodeID, nodeCount), nil
	}
}
