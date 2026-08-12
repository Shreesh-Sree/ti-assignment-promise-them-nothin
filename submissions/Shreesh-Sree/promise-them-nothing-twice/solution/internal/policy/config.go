// Package policy resolves, for a customer and a timestamp, what rate
// limit applies and why. It owns the config schema (tiers, customers,
// time-boxed overrides), loud startup/reload validation, and the
// mandatory-expiry rule from DESIGN-NOTES.md Part 1 §2. It has no HTTP
// and no coordination — those are internal/httpapi and
// internal/coordinator, built in later sessions.
package policy

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the full policy configuration: tiers, the customers mapped to
// them, and any time-boxed overrides. Once Validate has returned nil, a
// *Config is treated as immutable — Resolver never edits one in place, it
// swaps in a whole new one (see Resolver.Reload), so a request reading
// from a *Config can never see it half-updated.
type Config struct {
	Tiers     map[string]TierConfig `yaml:"tiers"`
	Customers []CustomerConfig      `yaml:"customers"`
	Overrides []OverrideConfig      `yaml:"overrides"`
}

// TierConfig is a shared rate limit tier. RPM of 0 means the tier has no
// shared default — "enterprise" is always negotiated per customer — and
// every customer on that tier must set LimitRPM explicitly instead.
type TierConfig struct {
	RPM int `yaml:"rpm"`
}

// CustomerConfig maps one customer to a tier, or to an explicit limit if
// their tier has none.
type CustomerConfig struct {
	ID       string `yaml:"id"`
	Tier     string `yaml:"tier"`
	LimitRPM int    `yaml:"limit_rpm,omitempty"`
}

// DailyWindow is a recurring daily UTC time-of-day window. StartUTC and
// EndUTC are "HH:MM" in 24-hour UTC and name the nominal, contracted
// window — the business fact. GraceMinutes pads enforcement past EndUTC;
// see the comment on OverrideConfig.instantsFor for why, and how the
// value should be chosen. Windows that cross midnight are not supported:
// nothing in this deployment needs one, and silently getting that wrong
// is worse than refusing to support it.
type DailyWindow struct {
	StartUTC     string `yaml:"start_utc"`
	EndUTC       string `yaml:"end_utc"`
	GraceMinutes int    `yaml:"grace_minutes"`
}

// OverrideConfig is a time-boxed, per-customer exception to their
// contracted limit. Expires is mandatory: Validate refuses to load a
// config where it's missing or already past, because an override with no
// forced expiry silently becomes the customer's permanent quota
// (DESIGN-NOTES.md Part 1 §2) — exactly the kind of undocumented standing
// bypass the CTO's "config and audit, not a midnight commit" rule exists
// to prevent.
type OverrideConfig struct {
	Customer string      `yaml:"customer"`
	LimitRPM int         `yaml:"limit_rpm"`
	Window   DailyWindow `yaml:"window"`
	Expires  string      `yaml:"expires"` // "YYYY-MM-DD", UTC
	Ticket   string      `yaml:"ticket"`
	Reason   string      `yaml:"reason"`

	// expiresAt is parsed and set by Validate, not by YAML unmarshaling —
	// Resolve checks it on every call (not just at load time), which is
	// what makes "expiry passes while the process keeps running" actually
	// take effect without a restart.
	expiresAt time.Time
}

// LoadConfig reads and parses (but does not validate) the config at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: reading config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("policy: parsing config %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks every rule this package enforces loudly: overrides must
// have a future expiry, must raise (never lower) the customer's
// contracted limit, must reference a real customer, and their window must
// parse. now is the reference instant "already in the past" is measured
// against — callers pass the resolver's clock, never time.Now() directly,
// so this stays testable without a real clock.
func (c *Config) Validate(now time.Time) error {
	if len(c.Tiers) == 0 {
		return fmt.Errorf("policy: config has no tiers")
	}

	customersByID := make(map[string]*CustomerConfig, len(c.Customers))
	for i := range c.Customers {
		cust := &c.Customers[i]
		if cust.ID == "" {
			return fmt.Errorf("policy: customer at index %d has no id", i)
		}
		if _, dup := customersByID[cust.ID]; dup {
			return fmt.Errorf("policy: duplicate customer id %q", cust.ID)
		}
		tier, ok := c.Tiers[cust.Tier]
		if !ok {
			return fmt.Errorf("policy: customer %q references undefined tier %q", cust.ID, cust.Tier)
		}
		if tier.RPM == 0 && cust.LimitRPM == 0 {
			return fmt.Errorf("policy: customer %q is on tier %q, which has no shared rpm, but sets no limit_rpm of its own", cust.ID, cust.Tier)
		}
		if tier.RPM != 0 && cust.LimitRPM != 0 {
			return fmt.Errorf("policy: customer %q sets limit_rpm but tier %q already has a shared rpm — set at most one", cust.ID, cust.Tier)
		}
		customersByID[cust.ID] = cust
	}

	for i := range c.Overrides {
		o := &c.Overrides[i]
		if o.Customer == "" {
			return fmt.Errorf("policy: override at index %d has no customer", i)
		}
		cust, ok := customersByID[o.Customer]
		if !ok {
			return fmt.Errorf("policy: override for %q references a customer that isn't configured", o.Customer)
		}
		if o.Ticket == "" {
			return fmt.Errorf("policy: override for %q has no ticket reference", o.Customer)
		}
		if o.Reason == "" {
			return fmt.Errorf("policy: override for %q has no reason", o.Customer)
		}
		if o.Expires == "" {
			return fmt.Errorf("policy: override for %q has no expiry — overrides must not be able to become permanent", o.Customer)
		}
		expiresAt, err := time.Parse("2006-01-02", o.Expires)
		if err != nil {
			return fmt.Errorf("policy: override for %q has an unparseable expiry %q: %w", o.Customer, o.Expires, err)
		}
		expiresAt = expiresAt.UTC()
		if !expiresAt.After(now) {
			return fmt.Errorf("policy: override for %q expires %s, which is not after the current time %s — refusing to start with an already-expired override",
				o.Customer, o.Expires, now.UTC().Format(time.RFC3339))
		}
		o.expiresAt = expiresAt

		contracted := contractedLimit(*cust, c.Tiers[cust.Tier])
		if o.LimitRPM <= contracted {
			return fmt.Errorf("policy: override for %q sets limit_rpm=%d, which does not raise the contracted limit of %d — overrides may only raise a limit",
				o.Customer, o.LimitRPM, contracted)
		}

		start, err := parseTimeOfDay(o.Window.StartUTC)
		if err != nil {
			return fmt.Errorf("policy: override for %q has invalid window.start_utc %q: %w", o.Customer, o.Window.StartUTC, err)
		}
		end, err := parseTimeOfDay(o.Window.EndUTC)
		if err != nil {
			return fmt.Errorf("policy: override for %q has invalid window.end_utc %q: %w", o.Customer, o.Window.EndUTC, err)
		}
		if end <= start {
			return fmt.Errorf("policy: override for %q has window.end_utc %q not after window.start_utc %q — overnight-spanning windows aren't supported",
				o.Customer, o.Window.EndUTC, o.Window.StartUTC)
		}
		if o.Window.GraceMinutes < 0 {
			return fmt.Errorf("policy: override for %q has a negative grace_minutes", o.Customer)
		}
	}

	return nil
}

// lookup returns the customer and their tier by ID.
func (c *Config) lookup(customerID string) (CustomerConfig, TierConfig, bool) {
	// Linear scan: fine at prototype scale (a handful of customers). Not
	// worth a map until the customer list is large enough to matter, and
	// nothing about correctness depends on which one this is.
	for _, cust := range c.Customers {
		if cust.ID == customerID {
			return cust, c.Tiers[cust.Tier], true
		}
	}
	return CustomerConfig{}, TierConfig{}, false
}

func contractedLimit(cust CustomerConfig, tier TierConfig) int {
	if cust.LimitRPM != 0 {
		return cust.LimitRPM
	}
	return tier.RPM
}

// parseTimeOfDay parses "HH:MM" into an offset from midnight.
func parseTimeOfDay(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}
