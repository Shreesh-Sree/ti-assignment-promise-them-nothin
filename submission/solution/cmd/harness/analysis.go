package main

import (
	"sort"
	"time"
)

// aggregate is the plain counts every scenario reports, computed once
// from a []Record so every scenario's numbers come from the same
// arithmetic rather than each reimplementing it slightly differently.
type aggregate struct {
	Sent             int
	Admitted         int
	Rejected         int
	Errored          int
	NodeDistribution map[string]int
	MaxRolling60s    int
	MaxWindowStart   time.Time
	MaxWindowEnd     time.Time
}

// summarize computes aggregate from raw records. Sent counts every
// request that got a real HTTP response (2xx or 429) — errored calls
// (connection refused, timeout) are counted separately, since a node
// being down mid-scenario (node-failure) should show up as errored, not
// silently vanish from the totals.
func summarize(records []Record) aggregate {
	a := aggregate{NodeDistribution: map[string]int{}}
	for _, r := range records {
		if r.Err != nil {
			a.Errored++
			continue
		}
		a.Sent++
		if r.NodeID != "" {
			a.NodeDistribution[r.NodeID]++
		}
		if r.Allowed {
			a.Admitted++
		} else {
			a.Rejected++
		}
	}
	a.MaxRolling60s, a.MaxWindowStart, a.MaxWindowEnd = maxRolling60s(records)
	return a
}

// maxRolling60s computes the true maximum number of admitted requests in
// any 60-second window, sliding over the actual client-observed arrival
// (ReceivedAt) timestamps — not bucketed to calendar minutes. This is
// deliberately the same definition internal/policy's "never exceeds
// quota" comment gives and the same method used to check Part 2's
// invariant against real data in session 6: for every admitted request's
// timestamp t, count admissions in (t-60s, t], and take the max over all
// of them. A fixed-window implementation could pass a naive per-minute
// check while failing this one; that's the entire point of computing it
// this way.
func maxRolling60s(records []Record) (max int, windowStart, windowEnd time.Time) {
	var times []time.Time
	for _, r := range records {
		if r.Err == nil && r.Allowed {
			times = append(times, r.ReceivedAt)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	for i, t := range times {
		lo := sort.Search(len(times), func(k int) bool {
			return !times[k].Before(t.Add(-60 * time.Second))
		})
		count := i - lo + 1
		if count > max {
			max = count
			windowStart = times[lo]
			windowEnd = t
		}
	}
	return max, windowStart, windowEnd
}

// perCalendarMinute buckets admitted requests by wall-clock minute, for
// the window-boundary scenario's informational display — showing that
// even individual calendar-minute buckets stay bounded is a sanity check,
// not the actual proof (maxRolling60s is), since a correct GCRA limiter
// bounds every 60-second window including calendar-aligned ones, so this
// alone can't distinguish correct from broken. It's shown so a reader can
// see admitted traffic actually crossed a minute boundary during the
// scenario, not just take that on faith.
func perCalendarMinute(records []Record) map[int64]int {
	buckets := map[int64]int{}
	for _, r := range records {
		if r.Err == nil && r.Allowed {
			buckets[r.ReceivedAt.Unix()/60] += 1
		}
	}
	return buckets
}
