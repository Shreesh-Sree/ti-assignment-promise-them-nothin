package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// serverLogLine is the shape internal/httpapi logs on every request —
// see server.go's "request_admission" slog.Info call. Parsed here purely
// to cross-check, never to decide PASS/FAIL: this harness's own
// client-side Records (client.go) are the primary, independent source of
// truth, per the explicit requirement that the harness not trust the
// server's own account of itself. This is the "even if it also
// cross-checks against the logs" half of that requirement.
//
// Time is the slog record's own timestamp — always real wall-clock,
// stamped by slog itself when Info() was called — used for matching
// against the harness's own real-time window. ArrivalTime is deliberately
// NOT used for that: it's the "now" GCRA actually decided against, which
// northwind-batch intentionally runs under a shifted dev-clock (see
// internal/policy/devclock.go) so the override window is reachable
// without waiting for real UTC 02:00. Comparing the harness's real-time
// window against a dev-clock-shifted arrival_time would silently match
// nothing — a real bug caught by actually running this cross-check
// against a dev-clock-shifted server, not a hypothetical one.
type serverLogLine struct {
	Time           string `json:"time"`
	Msg            string `json:"msg"`
	NodeID         string `json:"node_id"`
	CustomerID     string `json:"customer_id"`
	ArrivalTime    string `json:"arrival_time"`
	Allowed        bool   `json:"allowed"`
	NodeShareLimit int    `json:"node_share_limit"`
}

// crossCheckResult is intentionally a plain string, not a verdict — a
// mismatch here is worth surfacing to a human, but it's a second opinion
// on the harness's own primary measurement, not something that should
// silently override or gate the scenario's actual PASS/FAIL.
func crossCheckAgainstServerLogs(composeFile string, services []string, customerID string, windowStart, windowEnd time.Time, clientAdmitted int) string {
	total, admitted, err := fetchServerAdmittedCount(composeFile, services, customerID, windowStart, windowEnd)
	if err != nil {
		return fmt.Sprintf("cross-check skipped (%v)", err)
	}
	if admitted == clientAdmitted {
		return fmt.Sprintf("cross-check OK: server logs report %d admitted (of %d total) for this window, matching the harness's own client-side count of %d exactly", admitted, total, clientAdmitted)
	}
	return fmt.Sprintf("cross-check MISMATCH: server logs report %d admitted (of %d total), harness's own client-side count is %d — investigate before trusting either number blindly", admitted, total, clientAdmitted)
}

func fetchServerAdmittedCount(composeFile string, services []string, customerID string, windowStart, windowEnd time.Time) (total, admitted int, err error) {
	args := []string{"compose", "-f", composeFile, "logs", "--no-color", "--no-log-prefix"}
	args = append(args, services...)
	cmd := exec.Command("docker", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if runErr := cmd.Run(); runErr != nil {
		return 0, 0, fmt.Errorf("docker compose logs failed: %w", runErr)
	}

	startStr := windowStart.UTC().Format(time.RFC3339Nano)
	endStr := windowEnd.UTC().Format(time.RFC3339Nano)

	for _, line := range strings.Split(out.String(), "\n") {
		idx := strings.Index(line, "{")
		if idx == -1 {
			continue
		}
		var entry serverLogLine
		if jsonErr := json.Unmarshal([]byte(line[idx:]), &entry); jsonErr != nil {
			continue
		}
		if entry.Msg != "request_admission" || entry.CustomerID != customerID {
			continue
		}
		if entry.Time < startStr || entry.Time > endStr {
			continue
		}
		total++
		if entry.Allowed {
			admitted++
		}
	}
	return total, admitted, nil
}
