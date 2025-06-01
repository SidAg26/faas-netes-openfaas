package k8s

import "sync"

// SA - RoundRobinSelector manages round-robin selection for multiple keys.
type RoundRobinSelector struct {
    lock   sync.Mutex
    last   map[string]int
}

func NewRoundRobinSelector() *RoundRobinSelector {
    return &RoundRobinSelector{
        last: make(map[string]int),
    }
}

// Next returns the next index for the given key and total count.
// It ensures the index is always within [0, total).
func (rr *RoundRobinSelector) Next(key string, total int) int {
    rr.lock.Lock()
    defer rr.lock.Unlock()

    last, ok := rr.last[key]
    if !ok || last >= total || last < 0 {
        last = -1
    }
    next := (last + 1) % total
    rr.last[key] = next
    return next
}