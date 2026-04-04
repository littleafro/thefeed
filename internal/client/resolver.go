package client

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/sartoopjj/thefeed/internal/protocol"
)

// ResolverChecker periodically probes the fetcher's configured resolvers and
// updates the active (healthy) resolver pool. It replaces the old file/CIDR
// scanner — no file I/O; just a plain DNS probe on channel 0.
type ResolverChecker struct {
	fetcher *Fetcher
	timeout time.Duration
	logFunc LogFunc
	started atomic.Bool // guards against double-start
}

// NewResolverChecker creates a health checker for the resolvers in fetcher.
// timeout is the per-probe deadline; 0 uses a 15-second default.
func NewResolverChecker(fetcher *Fetcher, timeout time.Duration) *ResolverChecker {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &ResolverChecker{
		fetcher: fetcher,
		timeout: timeout,
	}
}

// SetLogFunc sets the callback used to emit health-check results to the log panel.
func (rc *ResolverChecker) SetLogFunc(fn LogFunc) {
	rc.logFunc = fn
}

// Start begins the periodic health-check loop in the background.
// An initial check runs immediately; subsequent checks happen every 10 minutes.
// ctx controls the lifetime — cancel it to stop the checker.
func (rc *ResolverChecker) Start(ctx context.Context) {
	rc.StartAndNotify(ctx, nil)
}

// StartAndNotify is like Start but calls onFirstDone (if non-nil) after the
// first successful health-check pass (i.e. at least one resolver is healthy),
// before the periodic ticker begins.
// If the initial scan finds zero healthy resolvers it retries every minute
// until at least one resolver becomes reachable (or ctx is cancelled).
// Safe to call only once per checker instance; subsequent calls are no-ops.
func (rc *ResolverChecker) StartAndNotify(ctx context.Context, onFirstDone func()) {
	if !rc.started.CompareAndSwap(false, true) {
		return // already started — prevent duplicate scan goroutines
	}
	go func() {
		// Keep scanning every minute until we find at least one healthy resolver.
		for {
			rc.CheckNow(ctx)
			if ctx.Err() != nil {
				return
			}
			if len(rc.fetcher.Resolvers()) > 0 {
				break // at least one resolver is up — proceed normally
			}
			rc.log("No healthy resolvers found — retrying in 1 minute...")
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Minute):
			}
		}

		if onFirstDone != nil && ctx.Err() == nil {
			onFirstDone()
		}

		// Periodic re-check every 30 minutes.
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rc.CheckNow(ctx)
				// If the periodic check leaves us with no resolvers,
				// fall back into the retry-every-minute loop.
				if ctx.Err() == nil && len(rc.fetcher.Resolvers()) == 0 {
					rc.log("All resolvers lost — scanning every minute until one recovers...")
					for {
						select {
						case <-ctx.Done():
							return
						case <-time.After(1 * time.Minute):
						}
						rc.CheckNow(ctx)
						if ctx.Err() != nil || len(rc.fetcher.Resolvers()) > 0 {
							break
						}
						rc.log("Still no healthy resolvers — retrying in 1 minute...")
					}
				}
			}
		}
	}()
}

// CheckNow runs a single resolver health-check pass immediately.
// ctx is used to abort in-flight probes early (e.g. when a profile is switched).
func (rc *ResolverChecker) CheckNow(ctx context.Context) {
	resolvers := rc.fetcher.AllResolvers()
	if len(resolvers) == 0 {
		return
	}

	total := len(resolvers)
	rc.log("RESOLVER_SCAN start %d", total)

	var healthy []string
	var mu sync.Mutex
	var done int
	wg := &sync.WaitGroup{}
	sem := make(chan struct{}, 10) // probe up to 10 resolvers concurrently

	for _, r := range resolvers {
		// Stop launching new probes if context was cancelled.
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ok := rc.checkOne(ctx, r)
			mu.Lock()
			if ok {
				healthy = append(healthy, r)
				rc.log("Resolver OK: %s", r)
			} else {
				rc.log("Resolver failed: %s", r)
			}
			done++
			rc.log("RESOLVER_SCAN progress %d/%d healthy=%d", done, total, len(healthy))
			mu.Unlock()
		}(r)
	}
	wg.Wait()

	if ctx.Err() != nil {
		return // context cancelled — don't update resolver list
	}

	rc.fetcher.SetActiveResolvers(healthy)
	if len(healthy) == 0 {
		rc.log("Resolver check done: 0/%d healthy", len(resolvers))
		rc.log("RESOLVER_SCAN done 0/%d", total)
		return
	}
	rc.log("Resolver check done: %d/%d healthy", len(healthy), len(resolvers))
	rc.log("RESOLVER_SCAN done %d/%d", len(healthy), total)
}

// checkOne probes a single resolver by sending a metadata channel query
// (channel 0, block 0). A resolver is considered healthy only if it returns
// a DNS response containing at least one TXT record that can be decoded with
// the fetcher's response key — the same bar as a real data fetch.
// This filters out resolvers that are reachable but strip TXT records, or
// that resolve the domain through a path that doesn't reach the thefeed server.
func (rc *ResolverChecker) checkOne(ctx context.Context, resolver string) bool {
	if !strings.Contains(resolver, ":") {
		resolver += ":53"
	}

	qname, err := protocol.EncodeQuery(
		rc.fetcher.queryKey,
		protocol.MetadataChannel, 0,
		rc.fetcher.domain,
		rc.fetcher.queryMode,
	)
	if err != nil {
		return false
	}

	probeCtx, cancel := context.WithTimeout(ctx, rc.timeout)
	defer cancel()

	c := &dns.Client{Timeout: rc.timeout}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), dns.TypeTXT)
	m.RecursionDesired = true
	m.SetEdns0(4096, false)

	start := time.Now()
	resp, _, err := c.ExchangeContext(probeCtx, m, resolver)
	latency := time.Since(start)
	if err != nil || resp == nil {
		rc.fetcher.RecordFailure(resolver)
		return false
	}

	// Require a decodable TXT record — same check as a real fetch.
	for _, ans := range resp.Answer {
		if txt, ok := ans.(*dns.TXT); ok {
			encoded := strings.Join(txt.Txt, "")
			if _, decErr := protocol.DecodeResponse(rc.fetcher.responseKey, encoded); decErr == nil {
				rc.fetcher.RecordSuccess(resolver, latency)
				return true
			}
		}
	}

	rc.fetcher.RecordFailure(resolver)
	return false
}

func (rc *ResolverChecker) log(format string, args ...any) {
	if rc.logFunc != nil {
		rc.logFunc(fmt.Sprintf(format, args...))
	}
}
