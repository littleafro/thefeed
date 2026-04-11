package client

import (
	"net"
	"testing"
)

func TestExpandTargets_SingleIP(t *testing.T) {
	ips, err := expandTargets([]string{"1.2.3.4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 || ips[0] != "1.2.3.4" {
		t.Errorf("got %v, want [1.2.3.4]", ips)
	}
}

func TestExpandTargets_MultipleIPs(t *testing.T) {
	ips, err := expandTargets([]string{"1.2.3.4", "5.6.7.8", "9.10.11.12"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 3 {
		t.Errorf("got %d IPs, want 3", len(ips))
	}
}

func TestExpandTargets_DeduplicatesIPs(t *testing.T) {
	ips, err := expandTargets([]string{"1.2.3.4", "1.2.3.4", "5.6.7.8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 2 {
		t.Errorf("got %d IPs, want 2 (dedup)", len(ips))
	}
}

func TestExpandTargets_SkipsEmpty(t *testing.T) {
	ips, err := expandTargets([]string{"", "  ", "1.2.3.4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 {
		t.Errorf("got %d IPs, want 1 (empty lines skipped)", len(ips))
	}
}

func TestExpandTargets_IPWithPort(t *testing.T) {
	// IP with port should be parsed by SplitHostPort fallback.
	// Note: this test may fail if DNS interception resolves "1.2.3.4:53" as a hostname.
	// We test with 127.0.0.1 which is less likely to be intercepted.
	ips, err := expandTargets([]string{"127.0.0.1:53"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 || ips[0] != "127.0.0.1" {
		t.Errorf("got %v, want [127.0.0.1]", ips)
	}
}

func TestExpandCIDR_Slash24(t *testing.T) {
	ips, err := expandCIDR("192.168.1.0/24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// /24 = 256 addresses, minus network (192.168.1.0) and broadcast (192.168.1.255) = 254
	if len(ips) != 254 {
		t.Errorf("got %d IPs, want 254", len(ips))
	}
	// Should not contain network or broadcast address.
	for _, ip := range ips {
		if ip == "192.168.1.0" {
			t.Error("should not contain network address 192.168.1.0")
		}
		if ip == "192.168.1.255" {
			t.Error("should not contain broadcast address 192.168.1.255")
		}
	}
	// First should be .1, last should be .254.
	if ips[0] != "192.168.1.1" {
		t.Errorf("first IP = %s, want 192.168.1.1", ips[0])
	}
	if ips[len(ips)-1] != "192.168.1.254" {
		t.Errorf("last IP = %s, want 192.168.1.254", ips[len(ips)-1])
	}
}

func TestExpandCIDR_Slash30(t *testing.T) {
	ips, err := expandCIDR("10.0.0.0/30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// /30 = 4 addresses, minus network and broadcast = 2
	if len(ips) != 2 {
		t.Errorf("got %d IPs, want 2", len(ips))
	}
}

func TestExpandCIDR_Slash32(t *testing.T) {
	ips, err := expandCIDR("10.0.0.5/32")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 || ips[0] != "10.0.0.5" {
		t.Errorf("got %v, want [10.0.0.5]", ips)
	}
}

func TestExpandCIDR_TooLarge(t *testing.T) {
	_, err := expandCIDR("10.0.0.0/8")
	if err == nil {
		t.Error("expected error for /8, got nil")
	}
}

func TestExpandCIDR_Slash16_Limit(t *testing.T) {
	ips, err := expandCIDR("10.0.0.0/16")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// /16 = 65536 addresses, minus network and broadcast = 65534
	if len(ips) != 65534 {
		t.Errorf("got %d IPs, want 65534", len(ips))
	}
}

func TestExpandCIDR_Invalid(t *testing.T) {
	_, err := expandCIDR("not-a-cidr")
	if err == nil {
		t.Error("expected error for invalid CIDR, got nil")
	}
}

func TestExpandTargets_CIDR(t *testing.T) {
	ips, err := expandTargets([]string{"10.0.0.0/30"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 2 {
		t.Errorf("got %d IPs, want 2", len(ips))
	}
}

func TestExpandTargets_Mixed(t *testing.T) {
	ips, err := expandTargets([]string{"1.2.3.4", "10.0.0.0/30"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 1 IP + 2 from /30 = 3
	if len(ips) != 3 {
		t.Errorf("got %d IPs, want 3", len(ips))
	}
}

func TestNewResolverScanner(t *testing.T) {
	rs := NewResolverScanner()
	if rs.state != ScannerIdle {
		t.Errorf("initial state = %s, want idle", rs.state)
	}
}

func TestScanner_Progress_Idle(t *testing.T) {
	rs := NewResolverScanner()
	p := rs.Progress()
	if p.State != ScannerIdle {
		t.Errorf("state = %s, want idle", p.State)
	}
	if p.Total != 0 || p.Scanned != 0 || p.Found != 0 {
		t.Errorf("expected all zeroes: total=%d scanned=%d found=%d", p.Total, p.Scanned, p.Found)
	}
}

func TestScanner_Start_NoTargets(t *testing.T) {
	rs := NewResolverScanner()
	err := rs.Start(ScannerConfig{
		Targets:    []string{},
		Passphrase: "test",
		Domain:     "example.com",
	})
	if err == nil {
		t.Error("expected error for empty targets")
	}
}

func TestScanner_Start_InvalidCIDR(t *testing.T) {
	rs := NewResolverScanner()
	err := rs.Start(ScannerConfig{
		Targets:    []string{"10.0.0.0/8"},
		Passphrase: "test",
		Domain:     "example.com",
	})
	if err == nil {
		t.Error("expected error for too-large CIDR")
	}
}

func TestScanner_StopIdle(t *testing.T) {
	rs := NewResolverScanner()
	// Stop on idle should not panic.
	rs.Stop()
	if rs.State() != ScannerDone {
		t.Errorf("state after stop = %s, want done", rs.State())
	}
}

func TestScanner_PauseResumeIdle(t *testing.T) {
	rs := NewResolverScanner()
	// Pause/Resume on idle should be no-ops.
	rs.Pause()
	if rs.State() != ScannerIdle {
		t.Errorf("state after pause on idle = %s, want idle", rs.State())
	}
	rs.Resume()
	if rs.State() != ScannerIdle {
		t.Errorf("state after resume on idle = %s, want idle", rs.State())
	}
}

func TestIncrementIP(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"1.2.3.4", "1.2.3.5"},
		{"1.2.3.255", "1.2.4.0"},
		{"1.2.255.255", "1.3.0.0"},
		{"255.255.255.255", "0.0.0.0"},
	}
	for _, tt := range tests {
		ip := parseIPv4(tt.in)
		incrementIP(ip)
		got := ip.String()
		if got != tt.want {
			t.Errorf("incrementIP(%s) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestIPToUint32(t *testing.T) {
	tests := []struct {
		in   string
		want uint32
	}{
		{"0.0.0.0", 0},
		{"0.0.0.1", 1},
		{"0.0.1.0", 256},
		{"1.0.0.0", 1 << 24},
		{"255.255.255.255", 0xFFFFFFFF},
	}
	for _, tt := range tests {
		ip := parseIPv4(tt.in)
		got := ipToUint32(ip)
		if got != tt.want {
			t.Errorf("ipToUint32(%s) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func parseIPv4(s string) net.IP {
	return net.ParseIP(s).To4()
}
