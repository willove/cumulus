package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// testPort is the session-face tests' stand-in for the store. They need to see
// the exact KV keys the suite composes and to inject a read failure mid-flight
// — neither of which an embedded engine exposes. Only the KV methods the
// session face calls are implemented: anything else falls through to the
// embedded cumulite.Port and panics loudly instead of quietly answering zero.
type testPort struct {
	cumulite.Port

	mu    sync.Mutex
	kv    map[string][]byte
	gets  []string
	puts  []string
	dels  []string
	lists []string // prefixes asked for

	// OnKVGet runs after the lookup is recorded and before its result is
	// returned. A non-nil error replaces the result, so a test can fail one
	// read or block inside it to interleave two requests.
	OnKVGet func(key string) error
}

func newTestPort() *testPort { return &testPort{kv: map[string][]byte{}} }

func (p *testPort) seed(key, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kv[key] = []byte(value)
}

// calls returns what the suite has asked the store for so far, per operation.
func (p *testPort) calls() (gets, puts, dels, lists []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := func(in []string) []string { return append([]string(nil), in...) }
	return cp(p.gets), cp(p.puts), cp(p.dels), cp(p.lists)
}

func (p *testPort) KVGet(_ context.Context, key string) ([]byte, error) {
	p.mu.Lock()
	p.gets = append(p.gets, key)
	v, ok := p.kv[key]
	p.mu.Unlock()
	// The hook runs outside the lock: a blocking hook must not stall another
	// request's read, or the interleave it is there to create cannot happen.
	if p.OnKVGet != nil {
		if err := p.OnKVGet(key); err != nil {
			return nil, err
		}
	}
	if !ok {
		return nil, contract.ErrNotFound
	}
	return v, nil
}

func (p *testPort) KVPut(_ context.Context, key string, value []byte, _ time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.puts = append(p.puts, key)
	p.kv[key] = append([]byte(nil), value...)
	return nil
}

func (p *testPort) KVDelete(_ context.Context, key string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dels = append(p.dels, key)
	_, ok := p.kv[key]
	delete(p.kv, key)
	return ok, nil
}

func (p *testPort) KVKeys(_ context.Context, prefix string, limit int) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lists = append(p.lists, prefix)
	var out []string
	for k := range p.kv {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
