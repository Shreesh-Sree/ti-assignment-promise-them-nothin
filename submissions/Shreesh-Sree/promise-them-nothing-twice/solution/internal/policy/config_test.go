package policy_test

import (
	"strings"
	"testing"
	"time"

	"relayapi/internal/policy"
)

func mustParse(t *testing.T, yamlContents string) *policy.Config {
	t.Helper()
	path := writeConfig(t, yamlContents)
	cfg, err := policy.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// TestValidateRejectsMissingExpiry: config fails validation loudly if an
// override has no expiry — "fail to start, don't warn."
func TestValidateRejectsMissingExpiry(t *testing.T) {
	cfg := mustParse(t, `
tiers:
  enterprise:
    rpm: 0
customers:
  - id: cust_x
    tier: enterprise
    limit_rpm: 300
overrides:
  - customer: cust_x
    limit_rpm: 1200
    window:
      start_utc: "02:00"
      end_utc: "04:00"
    ticket: "OPS-1"
    reason: "test"
`)
	err := cfg.Validate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("Validate: want error for missing expiry, got nil")
	}
	if !strings.Contains(err.Error(), "no expiry") {
		t.Errorf("Validate error = %q, want it to mention the missing expiry", err)
	}
}

// TestValidateRejectsExpiredOverride: config fails validation loudly if
// the expiry is already in the past relative to now.
func TestValidateRejectsExpiredOverride(t *testing.T) {
	cfg := mustParse(t, `
tiers:
  enterprise:
    rpm: 0
customers:
  - id: cust_x
    tier: enterprise
    limit_rpm: 300
overrides:
  - customer: cust_x
    limit_rpm: 1200
    window:
      start_utc: "02:00"
      end_utc: "04:00"
    expires: "2023-01-01"
    ticket: "OPS-1"
    reason: "test"
`)
	err := cfg.Validate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) // now is after the expiry
	if err == nil {
		t.Fatal("Validate: want error for an already-expired override, got nil")
	}
	if !strings.Contains(err.Error(), "not after the current time") {
		t.Errorf("Validate error = %q, want it to mention the expiry check", err)
	}
}

// TestValidateRejectsLoweringOverride: config fails validation loudly if
// an override lowers rather than raises the contracted limit.
func TestValidateRejectsLoweringOverride(t *testing.T) {
	cfg := mustParse(t, `
tiers:
  enterprise:
    rpm: 0
customers:
  - id: cust_x
    tier: enterprise
    limit_rpm: 300
overrides:
  - customer: cust_x
    limit_rpm: 100
    window:
      start_utc: "02:00"
      end_utc: "04:00"
    expires: "2099-01-01"
    ticket: "OPS-1"
    reason: "test"
`)
	err := cfg.Validate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("Validate: want error for an override that lowers the limit, got nil")
	}
	if !strings.Contains(err.Error(), "does not raise") {
		t.Errorf("Validate error = %q, want it to mention raising the limit", err)
	}
}

func TestValidateAcceptsWellFormedConfig(t *testing.T) {
	cfg := mustParse(t, testConfigYAML)
	if err := cfg.Validate(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Validate: want nil for a well-formed config, got %v", err)
	}
}

// TestRealCustomersYAMLIsValid guards against the checked-in config
// drifting out of sync with the rules Validate enforces.
func TestRealCustomersYAMLIsValid(t *testing.T) {
	cfg, err := policy.LoadConfig("../../configs/customers.yaml")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// A fixed instant well before the checked-in override's expiry, so
	// this test doesn't start failing on its own the day that date
	// arrives — TestValidateRejectsExpiredOverride already covers that
	// behavior directly, with a fixture that doesn't rot.
	if err := cfg.Validate(time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("configs/customers.yaml failed validation: %v", err)
	}
}
