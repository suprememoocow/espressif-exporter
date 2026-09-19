package registry

import (
	"net/netip"
	"slices"
)

// AddressPolicy decides which of a device's observed addresses the collectors dial.
type AddressPolicy struct {
	AllowIPv6 bool
	Allow     []netip.Prefix
	Deny      []netip.Prefix
}

// NewAddressPolicy parses CIDR strings that Validate has already checked.
func NewAddressPolicy(allowIPv6 bool, allow, deny []string) (AddressPolicy, error) {
	p := AddressPolicy{AllowIPv6: allowIPv6}
	for _, src := range []struct {
		in  []string
		out *[]netip.Prefix
	}{{allow, &p.Allow}, {deny, &p.Deny}} {
		for _, s := range src.in {
			pfx, err := netip.ParsePrefix(s)
			if err != nil {
				return AddressPolicy{}, err
			}
			*src.out = append(*src.out, pfx.Masked())
		}
	}
	return p, nil
}

// score ranks a candidate address. A negative score means unusable.
//
// The hard rejections are not stylistic. An IPv6 link-local address carries a host
// interface scope that does not exist inside the container's network namespace, so it is
// unroutable by construction. IPv6 is off by default for a related reason: a Docker
// bridge network usually has no IPv6 at all, so preferring a global IPv6 address would
// make an otherwise healthy device permanently unscrapeable.
func (p AddressPolicy) score(addr netip.Addr) int {
	if !addr.IsValid() {
		return -1
	}
	addr = addr.Unmap()

	switch {
	case addr.IsLoopback(), addr.IsMulticast(), addr.IsUnspecified():
		return -1
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		return -1 // fe80::/10 and 169.254.0.0/16
	case addr.IsInterfaceLocalMulticast():
		return -1
	}
	for _, d := range p.Deny {
		if d.Contains(addr) {
			return -1
		}
	}
	if addr.Is6() && !p.AllowIPv6 {
		return -1
	}

	inAllow := len(p.Allow) == 0
	for _, a := range p.Allow {
		if a.Contains(addr) {
			inAllow = true
			break
		}
	}
	if !inAllow {
		// Outside the allowlist an address is not rejected outright, only demoted: a
		// misconfigured allowlist should degrade reachability, not sever it.
		if addr.Is4() {
			return 10
		}
		return 5
	}

	switch {
	case addr.Is4() && len(p.Allow) > 0:
		return 100
	case addr.Is4():
		return 80
	default:
		return 40
	}
}

// Usable reports whether the address can be dialled at all.
func (p AddressPolicy) Usable(addr netip.Addr) bool { return p.score(addr) >= 0 }

// AddressCandidate is one observed address and whether the operator vouched for it.
type AddressCandidate struct {
	Addr    netip.Addr
	Trusted bool
}

// OrderCandidates ranks candidates, admitting operator-supplied addresses unconditionally
// and ranking them above anything merely discovered.
func (p AddressPolicy) OrderCandidates(candidates []AddressCandidate, pinned netip.Addr) (ordered, rejected []netip.Addr) {
	var trusted, discovered []netip.Addr
	for _, c := range candidates {
		switch {
		case c.Trusted:
			trusted = append(trusted, c.Addr.Unmap())
		case p.Usable(c.Addr):
			discovered = append(discovered, c.Addr.Unmap())
		default:
			rejected = append(rejected, c.Addr.Unmap())
		}
	}

	ordered = append(dedupe(trusted), p.Order(discovered, pinned)...)
	if pinned.IsValid() {
		if i := slices.Index(ordered, pinned.Unmap()); i > 0 {
			ordered = slices.Insert(slices.Delete(ordered, i, i+1), 0, pinned.Unmap())
		}
	}
	return ordered, rejected
}

func dedupe(addrs []netip.Addr) []netip.Addr {
	seen := make(map[netip.Addr]bool, len(addrs))
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// Order ranks candidates best-first, dropping unusable ones and de-duplicating.
//
// pinned, when valid and usable, is forced to the front. That stickiness is what turns a
// DHCP renewal into a single clean cutover instead of a thrash between a device's
// interfaces: the primary only moves once the pinned address has actually stopped
// working.
func (p AddressPolicy) Order(candidates []netip.Addr, pinned netip.Addr) []netip.Addr {
	type scored struct {
		addr  netip.Addr
		score int
	}

	seen := make(map[netip.Addr]bool, len(candidates))
	out := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		c = c.Unmap()
		if seen[c] {
			continue
		}
		seen[c] = true
		if s := p.score(c); s >= 0 {
			out = append(out, scored{c, s})
		}
	}

	slices.SortStableFunc(out, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return a.addr.Compare(b.addr) // deterministic tie-break
	})

	addrs := make([]netip.Addr, 0, len(out))
	for _, s := range out {
		addrs = append(addrs, s.addr)
	}

	if pinned.IsValid() {
		pinned = pinned.Unmap()
		if i := slices.Index(addrs, pinned); i > 0 {
			addrs = slices.Insert(slices.Delete(addrs, i, i+1), 0, pinned)
		}
	}
	return addrs
}
