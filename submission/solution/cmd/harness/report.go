package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// CustomerResult is one customer's row in a scenario's report — the unit
// the "plain stdout table per customer" requirement asks for.
type CustomerResult struct {
	CustomerID       string
	ContractedLimit  int // from configs/customers.yaml, read directly by the harness — independent of what the server reports
	EffectiveLimit   int // from the server's X-RateLimit-Limit header — the policy decision actually applied
	OfferedRPM       int
	Sent             int
	Admitted         int
	Rejected         int
	Errored          int
	MaxRolling60s    int
	NodeDistribution map[string]int
	Verdict          string // "PASS" or "FAIL" — exactly one of these two tokens, never a third
	Notes            []string
}

// ScenarioResult is one named scenario's full outcome: one or more
// customer rows, plus scenario-level notes that don't belong to any
// single customer (e.g. which phase northwind-batch detected, when
// node-failure killed a node).
type ScenarioResult struct {
	Name      string
	Customers []CustomerResult
	Notes     []string
}

// Verdict is FAIL if any customer row is FAIL — a scenario cannot pass
// while any of its customers failed.
func (s ScenarioResult) Verdict() string {
	for _, c := range s.Customers {
		if c.Verdict == "FAIL" {
			return "FAIL"
		}
	}
	return "PASS"
}

func printReport(results []ScenarioResult, asJSON bool) {
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
		return
	}

	for _, s := range results {
		printScenario(s)
	}
	printOverallSummary(results)
}

func printScenario(s ScenarioResult) {
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("SCENARIO: %s — %s\n", s.Name, s.Verdict())
	fmt.Println(strings.Repeat("=", 78))

	for _, n := range s.Notes {
		fmt.Printf("  * %s\n", n)
	}
	if len(s.Notes) > 0 {
		fmt.Println()
	}

	fmt.Printf("%-24s %10s %10s %8s %9s %9s %8s %16s %s\n",
		"customer", "contract", "effective", "offered", "admitted", "rejected", "errored", "max_roll_60s", "verdict")
	for _, c := range s.Customers {
		fmt.Printf("%-24s %10d %10d %8d %9d %9d %8d %16s %s\n",
			c.CustomerID, c.ContractedLimit, c.EffectiveLimit, c.OfferedRPM,
			c.Admitted, c.Rejected, c.Errored,
			fmt.Sprintf("%d/%d", c.MaxRolling60s, safetyBound(c.EffectiveLimit)),
			c.Verdict,
		)
		printNodeDistribution(c.NodeDistribution)
		for _, note := range c.Notes {
			fmt.Printf("    NOTE: %s\n", note)
		}
	}
	fmt.Println()
}

func printNodeDistribution(dist map[string]int) {
	if len(dist) == 0 {
		return
	}
	nodes := make([]string, 0, len(dist))
	for n := range dist {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, fmt.Sprintf("%s=%d", n, dist[n]))
	}
	fmt.Printf("    node distribution: %s\n", strings.Join(parts, "  "))
}

func printOverallSummary(results []ScenarioResult) {
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("OVERALL")
	fmt.Println(strings.Repeat("=", 78))
	allPass := true
	for _, s := range results {
		v := s.Verdict()
		if v == "FAIL" {
			allPass = false
		}
		fmt.Printf("  %-24s %s\n", s.Name, v)
	}
	if allPass {
		fmt.Println("\nALL SCENARIOS PASS")
	} else {
		fmt.Println("\nAT LEAST ONE SCENARIO FAILED")
	}
}

// anyFail reports whether the exit code should be non-zero.
func anyFail(results []ScenarioResult) bool {
	for _, s := range results {
		if s.Verdict() == "FAIL" {
			return true
		}
	}
	return false
}
