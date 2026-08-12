package audit_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"relayapi/internal/audit"
)

func TestOverrideAppliedEmitsAllRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	start := time.Date(2024, 1, 2, 2, 0, 0, 0, time.UTC)
	end := time.Date(2024, 1, 2, 5, 0, 0, 0, time.UTC)
	audit.OverrideApplied(logger, "cust_northwind_logistics", 300, 1200, "OPS-4821", start, end)

	out := buf.String()
	for _, want := range []string{
		"event=override_applied",
		"customer_id=cust_northwind_logistics",
		"contracted_limit_rpm=300",
		"effective_limit_rpm=1200",
		"override_ticket=OPS-4821",
		"window_start=",
		"window_end=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit event missing %q:\n%s", want, out)
		}
	}
}
