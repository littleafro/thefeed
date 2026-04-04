package client

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/sartoopjj/thefeed/internal/protocol"
)

// mockExchange returns a factory for exchangeFn that records calls and
// returns either a successful TXT response (encoded payload) or an error.
//
// When payload is non-nil the mock builds a valid encrypted TXT record using
// the fetcher's responseKey so that queryResolver can decode it correctly.
// When payload is nil the mock returns errFn(addr).
func mockExchange(f *Fetcher, payload []byte, errFn func(addr string) error) func(context.Context, *dns.Msg, string) (*dns.Msg, time.Duration, error) {
	return func(ctx context.Context, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if errFn != nil {
			if err := errFn(addr); err != nil {
				return nil, 0, err
			}
		}
		resp := new(dns.Msg)
		resp.SetReply(m)
		resp.Rcode = dns.RcodeSuccess
		if payload != nil {
			encoded, encErr := protocol.EncodeResponse(f.responseKey, payload, 0)
			if encErr != nil {
				return nil, 0, encErr
			}
			resp.Answer = []dns.RR{&dns.TXT{
				Hdr: dns.RR_Header{Name: m.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 0},
				Txt: []string{encoded},
			}}
		}
		return resp, time.Millisecond, nil
	}
}

func newTestFetcher(t *testing.T, resolvers []string) *Fetcher {
	t.Helper()
	f, err := NewFetcher("t.example.com", "test-passphrase", resolvers)
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	// Simulate the resolver scanner having validated all provided resolvers.
	f.SetActiveResolvers(resolvers)
	// Block all real DNS traffic by default.
	f.exchangeFn = func(_ context.Context, _ *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
		return nil, 0, fmt.Errorf("real DNS blocked in tests (resolver: %s)", addr)
	}
	return f
}

func TestSetActiveResolversAllowsEmpty(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	f.SetActiveResolvers(nil)
	if got := f.Resolvers(); len(got) != 0 {
		t.Fatalf("len(Resolvers()) = %d, want 0", len(got))
	}
}

func TestSetActiveResolversReplacesPool(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	f.SetActiveResolvers([]string{"9.9.9.9:53"})
	got := f.Resolvers()
	if len(got) != 1 || got[0] != "9.9.9.9:53" {
		t.Fatalf("Resolvers() = %v, want [9.9.9.9:53]", got)
	}
}

// TestResolverScoreNoData checks that a resolver with no recorded stats gets neutral weight 1.0.
func TestResolverScoreNoData(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	if got := f.resolverScore("1.1.1.1:53"); got != 1.0 {
		t.Fatalf("resolverScore with no data = %v, want 1.0", got)
	}
}

// TestResolverScoreSuccessBeatsFailure checks that a 100% success resolver
// scores higher than a 100% failure resolver.
func TestResolverScoreSuccessBeatsFailure(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	f.RecordSuccess("1.1.1.1:53", 50*time.Millisecond)
	f.RecordFailure("8.8.8.8:53")
	good := f.resolverScore("1.1.1.1:53")
	bad := f.resolverScore("8.8.8.8:53")
	if good <= bad {
		t.Fatalf("expected good resolver (%v) to score higher than bad (%v)", good, bad)
	}
}

// TestResolverScoreFasterBeatsSlower checks that an equal-success resolver
// with lower latency scores higher.
func TestResolverScoreFasterBeatsSlower(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	f.RecordSuccess("1.1.1.1:53", 10*time.Millisecond)
	f.RecordSuccess("8.8.8.8:53", 500*time.Millisecond)
	fast := f.resolverScore("1.1.1.1:53")
	slow := f.resolverScore("8.8.8.8:53")
	if fast <= slow {
		t.Fatalf("expected fast resolver (%v) to score higher than slow (%v)", fast, slow)
	}
}

// TestPickWeightedResolversReturnsN checks that pickWeightedResolvers returns
// at most n distinct resolvers.
func TestPickWeightedResolversReturnsN(t *testing.T) {
	resolvers := []string{"1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53", "208.67.222.222:53"}
	f := newTestFetcher(t, resolvers)
	f.SetActiveResolvers(resolvers)
	picked := f.pickWeightedResolvers(2)
	if len(picked) != 2 {
		t.Fatalf("pickWeightedResolvers(2) returned %d items, want 2", len(picked))
	}
	seen := map[string]bool{}
	for _, r := range picked {
		if seen[r] {
			t.Fatalf("pickWeightedResolvers returned duplicate resolver %s", r)
		}
		seen[r] = true
	}
}

// TestPickWeightedResolversMoreThanAvailable returns all when n > pool size.
func TestPickWeightedResolversMoreThanAvailable(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	picked := f.pickWeightedResolvers(5)
	if len(picked) != 1 {
		t.Fatalf("expected 1 resolver when pool has 1, got %d", len(picked))
	}
}

// TestScatterQuerySuccess checks that scatterQuery returns data when
// the mock exchange responds successfully.
func TestScatterQuerySuccess(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	want := []byte("hello")
	f.exchangeFn = mockExchange(f, want, nil)

	ctx := context.Background()
	got, err := f.scatterQuery(ctx, []string{"1.1.1.1:53"}, "test.t.example.com.")
	if err != nil {
		t.Fatalf("scatterQuery: unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("scatterQuery returned %q, want %q", got, want)
	}
}

// TestScatterQueryUsesFirstResponse checks that when multiple resolvers respond,
// the first successful answer wins and the call returns without error.
func TestScatterQueryUsesFirstResponse(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	want := []byte("winner")
	f.exchangeFn = mockExchange(f, want, nil)

	ctx := context.Background()
	got, err := f.scatterQuery(ctx, []string{"1.1.1.1:53", "8.8.8.8:53"}, "test.t.example.com.")
	if err != nil {
		t.Fatalf("scatterQuery: unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("scatterQuery returned %q, want %q", got, want)
	}
}

// TestScatterQueryAllFail checks that scatterQuery returns an error when
// all resolvers fail.
func TestScatterQueryAllFail(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53", "8.8.8.8:53"})
	f.exchangeFn = mockExchange(f, nil, func(addr string) error {
		return fmt.Errorf("connection refused from %s", addr)
	})

	ctx := context.Background()
	_, err := f.scatterQuery(ctx, []string{"1.1.1.1:53", "8.8.8.8:53"}, "test.t.example.com.")
	if err == nil {
		t.Fatal("expected error when all resolvers fail, got nil")
	}
}

// TestScatterQueryContextCancel checks that scatterQuery respects context cancellation.
func TestScatterQueryContextCancel(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	// Block forever until context is cancelled.
	f.exchangeFn = func(ctx context.Context, _ *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.scatterQuery(ctx, []string{"1.1.1.1:53"}, "test.t.example.com.")
	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
}

// TestSetScatter validates that SetScatter clamps values < 1 to 1.
func TestSetScatter(t *testing.T) {
	f := newTestFetcher(t, []string{"1.1.1.1:53"})
	f.SetScatter(0) // should clamp to 1
	if f.scatter != 1 {
		t.Fatalf("scatter = %d after SetScatter(0), want 1", f.scatter)
	}
	f.SetScatter(3)
	if f.scatter != 3 {
		t.Fatalf("scatter = %d after SetScatter(3), want 3", f.scatter)
	}
}
