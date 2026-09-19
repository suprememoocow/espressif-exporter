package registry

import (
	"net/netip"
	"testing"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, 0, len(ss))
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestAddressPolicyRejections(t *testing.T) {
	p, err := NewAddressPolicy(false, nil, []string{"10.99.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	rejected := []string{
		"127.0.0.1",    // loopback
		"169.254.10.4", // IPv4 link-local
		"fe80::1",      // IPv6 link-local: its interface scope does not exist in the container
		"224.0.0.251",  // multicast
		"0.0.0.0",      // unspecified
		"10.99.1.1",    // explicitly denied
		"2001:db8::1",  // IPv6 while AllowIPv6 is false
	}
	for _, s := range rejected {
		if p.Usable(netip.MustParseAddr(s)) {
			t.Errorf("%s should be unusable", s)
		}
	}
	if !p.Usable(netip.MustParseAddr("192.168.1.57")) {
		t.Error("192.168.1.57 should be usable")
	}
}

func TestAddressPolicyPrefersAllowlistedIPv4(t *testing.T) {
	p, err := NewAddressPolicy(true, []string{"192.168.1.0/24"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Order(addrs("2001:db8::5", "10.0.0.9", "192.168.1.57"), netip.Addr{})
	want := addrs("192.168.1.57", "10.0.0.9", "2001:db8::5")
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A pinned address is the one that last produced a successful scrape. Keeping it at the
// front is what stops the exporter flapping between a device's two interfaces.
func TestAddressPolicyPinnedWins(t *testing.T) {
	p, _ := NewAddressPolicy(false, []string{"192.168.1.0/24"}, nil)
	pinned := netip.MustParseAddr("10.0.0.9")

	got := p.Order(addrs("192.168.1.57", "10.0.0.9"), pinned)
	if got[0] != pinned {
		t.Errorf("got %v, want the pinned address %v first", got, pinned)
	}

	// A pinned address that has become unusable must not resurrect itself.
	got = p.Order(addrs("192.168.1.57"), netip.MustParseAddr("127.0.0.1"))
	if len(got) != 1 || got[0] != netip.MustParseAddr("192.168.1.57") {
		t.Errorf("got %v, want only 192.168.1.57", got)
	}
}

func TestAddressPolicyDeduplicates(t *testing.T) {
	p, _ := NewAddressPolicy(false, nil, nil)
	got := p.Order(addrs("192.168.1.57", "192.168.1.57", "192.168.1.58"), netip.Addr{})
	if len(got) != 2 {
		t.Errorf("got %v, want 2 unique addresses", got)
	}
}

// An address outside the allowlist is demoted rather than dropped, so a misconfigured
// allowlist degrades reachability instead of severing it.
func TestAddressPolicyAllowlistDemotesRatherThanRejects(t *testing.T) {
	p, _ := NewAddressPolicy(false, []string{"192.168.1.0/24"}, nil)
	got := p.Order(addrs("10.0.0.9"), netip.Addr{})
	if len(got) != 1 {
		t.Fatalf("got %v, want the non-allowlisted address retained", got)
	}
}

// An operator-supplied address is an assertion, not a hint. Second-guessing it produces
// a device that silently never scrapes, which is much harder to diagnose than a bad
// config.
func TestTrustedAddressesBypassTheFilters(t *testing.T) {
	p, _ := NewAddressPolicy(false, []string{"192.168.1.0/24"}, nil)

	ordered, rejected := p.OrderCandidates([]AddressCandidate{
		{Addr: netip.MustParseAddr("127.0.0.1"), Trusted: true},
	}, netip.Addr{})

	if len(ordered) != 1 || ordered[0].String() != "127.0.0.1" {
		t.Errorf("ordered = %v, want the trusted loopback address retained", ordered)
	}
	if len(rejected) != 0 {
		t.Errorf("rejected = %v, want none", rejected)
	}
}

// A discovered address that cannot be routed is reported, so an empty address list has
// a visible cause rather than being a silent no_address on every probe.
func TestRejectedAddressesAreReported(t *testing.T) {
	p, _ := NewAddressPolicy(false, nil, nil)

	ordered, rejected := p.OrderCandidates([]AddressCandidate{
		{Addr: netip.MustParseAddr("fe80::1")},
		{Addr: netip.MustParseAddr("169.254.3.4")},
	}, netip.Addr{})

	if len(ordered) != 0 {
		t.Errorf("ordered = %v, want none usable", ordered)
	}
	if len(rejected) != 2 {
		t.Errorf("rejected = %v, want both link-local addresses reported", rejected)
	}
}

// A trusted address outranks a discovered one, so an explicit override actually wins.
func TestTrustedOutranksDiscovered(t *testing.T) {
	p, _ := NewAddressPolicy(false, nil, nil)

	ordered, _ := p.OrderCandidates([]AddressCandidate{
		{Addr: netip.MustParseAddr("192.168.1.57")},
		{Addr: netip.MustParseAddr("10.0.0.9"), Trusted: true},
	}, netip.Addr{})

	if len(ordered) != 2 || ordered[0].String() != "10.0.0.9" {
		t.Errorf("ordered = %v, want the trusted address first", ordered)
	}
}
