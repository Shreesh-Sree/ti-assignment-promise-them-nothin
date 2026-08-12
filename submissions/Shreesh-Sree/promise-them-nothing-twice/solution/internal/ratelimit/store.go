package ratelimit

import (
	"hash/fnv"
	"sync"
	"time"
)

// numShards is the number of independent locks the customer state map is
// split across. It doesn't need to be large for a prototype — it needs to
// be more than one, so that customer A's traffic contending for its shard
// doesn't add latency to customer B's requests. A single global mutex
// would still produce correct counts, but it makes every customer's
// request path depend on every other customer's request rate, which is a
// violation of per-customer isolation in spirit even when the numbers
// come out right.
const numShards = 32

// store is a striped-lock map of per-customer GCRA state (TAT, the
// theoretical arrival time). Two customers whose keys land on different
// shards never block each other. Two customers that happen to hash to the
// same shard share a mutex, but the critical section is a single map
// read/write plus a few arithmetic operations — not a source of
// meaningful contention even when it happens, and unrelated to whether
// their counts stay correct, which store.go does not affect either way.
type store struct {
	shards [numShards]*shard
}

type shard struct {
	mu    sync.Mutex
	state map[string]time.Time
}

func newStore() *store {
	s := &store{}
	for i := range s.shards {
		s.shards[i] = &shard{state: make(map[string]time.Time)}
	}
	return s
}

func (s *store) shardFor(key string) *shard {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key)) // fnv.Write never returns an error
	return s.shards[h.Sum32()%numShards]
}

// withTAT runs fn under the lock for key's shard, passing it the current
// TAT (the zero value if key has never been seen before) and persisting
// whatever TAT fn returns. fn must be pure and fast — the shard's mutex is
// held for the duration of the call, so it must never do I/O or block.
func (s *store) withTAT(key string, fn func(tat time.Time) (Decision, time.Time)) Decision {
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	decision, newTAT := fn(sh.state[key])
	sh.state[key] = newTAT
	return decision
}
