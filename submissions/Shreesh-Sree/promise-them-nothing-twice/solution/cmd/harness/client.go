// Command harness is RelayAPI's verification harness — the thing
// DESIGN-NOTES.md and DECISIONS.md call the first-class deliverable: a
// reviewer should be able to tell correct behavior from incorrect
// behavior by reading this program's output alone, without opening
// internal/ratelimit, internal/coordinator, or internal/policy.
//
// This file is the client engine session 5's cmd/loadgen used to be:
// paced, keep-alive HTTP load offered at a fixed customer. It is now one
// building block scenarios.go composes, not a second, overlapping tool —
// cmd/loadgen no longer exists.
package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Record is one request's real outcome, timestamped independently on the
// client side. This is the harness's own account, not the server's: it
// never reads internal/httpapi's request_admission logs to decide
// PASS/FAIL (crosscheck.go optionally reads them afterward, to compare
// against this independent record, not to replace it).
type Record struct {
	SentAt     time.Time
	ReceivedAt time.Time
	NodeID     string
	StatusCode int
	Allowed    bool // true iff StatusCode == 200
	Err        error
}

// OfferConfig parameterizes one paced traffic offer against one customer.
type OfferConfig struct {
	Client      *http.Client
	URL         string
	CustomerID  string
	RPM         int
	Duration    time.Duration
	Concurrency int
}

// Offer sends RPM-paced GET requests carrying X-Customer-Id: CustomerID
// to URL for Duration, using Concurrency persistent keep-alive workers
// (the same shape session 5 established: a single global ticker feeding a
// buffered channel, so total offered rate is exact regardless of
// concurrency, and workers reuse connections instead of opening one per
// request). Returns every request's outcome — not just aggregate counts —
// so the caller can compute a real rolling-window admitted count instead
// of trusting a summary.
func Offer(ctx context.Context, cfg OfferConfig) []Record {
	interval := time.Minute / time.Duration(cfg.RPM)

	records := make(chan Record, cfg.Concurrency*4)
	requests := make(chan struct{}, cfg.Concurrency*2)

	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requests {
				records <- doRequest(cfg.Client, cfg.URL, cfg.CustomerID)
			}
		}()
	}

	collected := make([]Record, 0, cfg.RPM*int(cfg.Duration.Seconds())/60+16)
	done := make(chan struct{})
	go func() {
		for r := range records {
			collected = append(collected, r)
		}
		close(done)
	}()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.Now().Add(cfg.Duration)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			goto drain
		case <-ticker.C:
			select {
			case requests <- struct{}{}:
			default:
				// Workers saturated (a request is taking longer than the
				// offered interval) — this tick is dropped, i.e. offered
				// but never sent. That gap between offered and Sent in the
				// report is itself informative, not hidden.
			}
		}
	}
drain:
	close(requests)
	wg.Wait()
	close(records)
	<-done

	return collected
}

func doRequest(client *http.Client, url, customerID string) Record {
	sentAt := time.Now()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return Record{SentAt: sentAt, ReceivedAt: time.Now(), Err: err}
	}
	req.Header.Set("X-Customer-Id", customerID)

	resp, err := client.Do(req)
	receivedAt := time.Now()
	if err != nil {
		return Record{SentAt: sentAt, ReceivedAt: receivedAt, Err: err}
	}
	defer resp.Body.Close()

	return Record{
		SentAt:     sentAt,
		ReceivedAt: receivedAt,
		NodeID:     resp.Header.Get("X-Node-Id"),
		StatusCode: resp.StatusCode,
		Allowed:    resp.StatusCode == http.StatusOK,
	}
}

// newHTTPClient builds the keep-alive client every scenario shares —
// MaxIdleConnsPerHost sized to concurrency so workers actually reuse
// connections instead of opening a fresh one per request, the same
// traffic shape session 5's numbers depend on being real.
func newHTTPClient(concurrency int) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: concurrency,
		},
	}
}
