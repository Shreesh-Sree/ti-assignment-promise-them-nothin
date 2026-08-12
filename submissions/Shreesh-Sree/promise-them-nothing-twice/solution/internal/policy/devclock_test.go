package policy_test

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"relayapi/internal/policy"
	"relayapi/internal/ratelimit"
)

func TestNewClockFromEnvDefaultsToRealClockWhenUnset(t *testing.T) {
	t.Setenv(policy.EnvDevClockAsOf, "")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	clock := policy.NewClockFromEnv(logger)

	if _, ok := clock.(ratelimit.RealClock); !ok {
		t.Errorf("NewClockFromEnv with unset env var: got %T, want ratelimit.RealClock", clock)
	}
	if buf.Len() != 0 {
		t.Errorf("NewClockFromEnv with unset env var logged something, want silence:\n%s", buf.String())
	}
}

func TestNewClockFromEnvAppliesOffsetAndWarnsWhenSet(t *testing.T) {
	target := time.Date(2026, 1, 1, 2, 30, 0, 0, time.UTC)
	t.Setenv(policy.EnvDevClockAsOf, target.Format(time.RFC3339))
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	clock := policy.NewClockFromEnv(logger)

	got := clock.Now()
	if diff := got.Sub(target); diff < 0 || diff > 2*time.Second {
		t.Errorf("Now() = %v, want within a couple seconds of %v", got, target)
	}
	if !strings.Contains(buf.String(), "DEV CLOCK OVERRIDE ACTIVE") {
		t.Errorf("expected a loud warning when the dev clock is active, got:\n%s", buf.String())
	}
}

func TestNewClockFromEnvPanicsOnMalformedValue(t *testing.T) {
	t.Setenv(policy.EnvDevClockAsOf, "not-a-timestamp")
	defer func() {
		if recover() == nil {
			t.Fatal("NewClockFromEnv: want a panic on a malformed value, got none")
		}
	}()
	policy.NewClockFromEnv(slog.New(slog.NewTextHandler(os.Stderr, nil)))
}
