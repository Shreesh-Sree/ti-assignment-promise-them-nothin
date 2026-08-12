package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"relayapi/internal/policy"
)

var registry = map[string]func(context.Context, *Env) ScenarioResult{
	"two-tenants-fair":  scenarioTwoTenantsFair,
	"over-limit-cutoff": scenarioOverLimitCutoff,
	"window-boundary":   scenarioWindowBoundary,
	"northwind-batch":   scenarioNorthwindBatch,
	"node-failure":      scenarioNodeFailure,
}

// scenarioOrder is fixed rather than derived from map iteration (which
// Go deliberately randomizes) so output is reproducible run to run, and
// so node-failure — the one scenario that leaves the stack in a changed
// state until its own revive command runs — is last by default.
var scenarioOrder = []string{
	"two-tenants-fair",
	"over-limit-cutoff",
	"window-boundary",
	"northwind-batch",
	"node-failure",
}

func main() {
	baseURL := flag.String("url", "http://localhost:8080", "RelayAPI base URL (the nginx-fronted address, not one node directly)")
	configPath := flag.String("config", "../configs/customers.yaml", "path to the policy config, read directly for contracted limits (independent of what the server reports)")
	scenariosFlag := flag.String("scenarios", "all", "comma-separated scenario names to run, or \"all\"")
	asJSON := flag.Bool("json", false, "emit JSON instead of the plain-text table")
	killCmd := flag.String("kill-cmd", "", "command run mid-way through node-failure, e.g. \"docker compose -f docker-compose.yml stop node2\" (argv-split, no shell)")
	reviveCmd := flag.String("revive-cmd", "", "command run at the end of node-failure to restore the stack, e.g. \"docker compose -f docker-compose.yml start node2\"")
	killAt := flag.Duration("kill-at", 15*time.Second, "offset into node-failure's run at which --kill-cmd fires")
	composeFile := flag.String("compose-file", "", "if set, enables the optional server-log cross-check via `docker compose -f <file> logs`")
	services := flag.String("services", "node1,node2,node3", "comma-separated node service names for the cross-check")
	timeout := flag.Duration("timeout", 6*time.Minute, "overall timeout for the whole run")
	flag.Parse()

	cfg, err := policy.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness: failed to load %s for contracted-limit lookups: %v\n", *configPath, err)
		os.Exit(2)
	}
	contracted := map[string]int{}
	for _, cust := range cfg.Customers {
		if cust.LimitRPM != 0 {
			contracted[cust.ID] = cust.LimitRPM
		} else if tier, ok := cfg.Tiers[cust.Tier]; ok {
			contracted[cust.ID] = tier.RPM
		}
	}

	env := &Env{
		BaseURL:         *baseURL,
		ContractedLimit: contracted,
		KillCmd:         *killCmd,
		ReviveCmd:       *reviveCmd,
		KillAt:          *killAt,
		ComposeFile:     *composeFile,
		Services:        splitNonEmpty(*services),
	}

	names := scenarioOrder
	if *scenariosFlag != "all" {
		names = splitNonEmpty(*scenariosFlag)
	}
	for _, n := range names {
		if _, ok := registry[n]; !ok {
			fmt.Fprintf(os.Stderr, "harness: unknown scenario %q — known scenarios: %s\n", n, strings.Join(scenarioOrder, ", "))
			os.Exit(2)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var results []ScenarioResult
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "harness: running %s...\n", n)
		results = append(results, registry[n](ctx, env))
	}

	printReport(results, *asJSON)

	if anyFail(results) {
		os.Exit(1)
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
