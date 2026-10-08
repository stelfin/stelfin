package egress

import (
	"fmt"
	"net/netip"
)

// Which addresses stelfin will connect to on an org's say-so.
//
// The question is not "is this address private". It is "could a request to this
// address reach something that trusts stelfin for being where it is" — the
// cloud metadata service that hands out the instance's credentials, the
// database listening on the pod network, the admin port bound to localhost.
// Every range below is one where the answer is yes, or where no public service
// can live and so there is no legitimate reason to ask.
//
// IPv4 is a deny-list, because nearly all of it is public and the exceptions
// are few and stable. IPv6 is an allow-list with exceptions, because nearly all
// of it is unallocated: only 2000::/3 is global unicast, so anything outside it
// is refused without needing a reason, and the handful of special-purpose
// blocks inside it are named.

// refusal is one range and why it is refused.
type refusal struct {
	prefix netip.Prefix
	why    string
}

// refused4 are the IPv4 ranges stelfin does not dial.
var refused4 = []refusal{
	{netip.MustParsePrefix("0.0.0.0/8"), "it is the unspecified network, which a host reads as itself"},
	{netip.MustParsePrefix("10.0.0.0/8"), "it is a private network"},
	{netip.MustParsePrefix("100.64.0.0/10"), "it is carrier-grade NAT space, which is private to whoever runs it"},
	{netip.MustParsePrefix("127.0.0.0/8"), "it is loopback"},
	// The range that makes this whole package necessary: 169.254.169.254 is
	// where a cloud instance fetches its own credentials, with no
	// authentication beyond being asked from the inside.
	{netip.MustParsePrefix("169.254.0.0/16"), "it is link-local, where a cloud's metadata service lives"},
	{netip.MustParsePrefix("172.16.0.0/12"), "it is a private network"},
	{netip.MustParsePrefix("192.0.0.0/24"), "it is reserved for protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "it is a documentation range"},
	{netip.MustParsePrefix("192.88.99.0/24"), "it is the retired 6to4 relay range"},
	{netip.MustParsePrefix("192.168.0.0/16"), "it is a private network"},
	{netip.MustParsePrefix("198.18.0.0/15"), "it is a benchmarking range"},
	{netip.MustParsePrefix("198.51.100.0/24"), "it is a documentation range"},
	{netip.MustParsePrefix("203.0.113.0/24"), "it is a documentation range"},
	{netip.MustParsePrefix("224.0.0.0/4"), "it is multicast"},
	// Includes 255.255.255.255, the limited broadcast address.
	{netip.MustParsePrefix("240.0.0.0/4"), "it is reserved, or broadcast"},
}

// global6 is the only part of IPv6 that holds public unicast addresses.
var global6 = netip.MustParsePrefix("2000::/3")

// refused6 are the IPv6 ranges stelfin does not dial.
//
// Several of these lie outside 2000::/3 and would be refused anyway. They are
// listed so that the refusal can say why, and so that a later widening of the
// allow-list does not quietly re-admit them.
var refused6 = []refusal{
	{netip.MustParsePrefix("::/128"), "it is the unspecified address"},
	{netip.MustParsePrefix("::1/128"), "it is loopback"},
	// The three below are the ways an IPv6 address carries an IPv4 one. Each
	// is a route by which 10.0.0.1 or 169.254.169.254 could be spelt as
	// something this list does not otherwise recognise, so the whole prefix
	// is refused rather than the embedded address being dug out and checked:
	// a public service has no reason to be reached through a translation
	// prefix, and a parser for three embeddings is three places to be wrong.
	{netip.MustParsePrefix("::/96"), "it is an IPv4-compatible address, which embeds an IPv4 one"},
	{netip.MustParsePrefix("::ffff:0:0/96"), "it is an IPv4-mapped address"},
	{netip.MustParsePrefix("64:ff9b::/96"), "it is the NAT64 prefix, which embeds an IPv4 address"},
	{netip.MustParsePrefix("64:ff9b:1::/48"), "it is the local NAT64 prefix, which embeds an IPv4 address"},
	{netip.MustParsePrefix("100::/64"), "it is the discard prefix"},
	// 2001::/23 is the IETF's special-purpose block. Teredo (2001::/32), which
	// embeds an IPv4 address, is the member that matters; the rest —
	// benchmarking, ORCHID, anycast for protocols nobody serves a webhook
	// from — are refused with it.
	{netip.MustParsePrefix("2001::/23"), "it is a special-purpose range, which includes Teredo"},
	{netip.MustParsePrefix("2001:db8::/32"), "it is a documentation range"},
	{netip.MustParsePrefix("2002::/16"), "it is the 6to4 prefix, which embeds an IPv4 address"},
	{netip.MustParsePrefix("3fff::/20"), "it is a documentation range"},
	{netip.MustParsePrefix("5f00::/16"), "it is reserved for segment routing"},
	{netip.MustParsePrefix("fc00::/7"), "it is a unique-local network, the IPv6 private range"},
	{netip.MustParsePrefix("fe80::/10"), "it is link-local"},
	{netip.MustParsePrefix("fec0::/10"), "it is the retired site-local range"},
	{netip.MustParsePrefix("ff00::/8"), "it is multicast"},
}

// CheckAddr refuses an address that is not public unicast.
//
// This is the check that counts. It is applied to the address a connection is
// about to be made to, after resolution, so it does not matter how the address
// was spelt or what name pointed at it.
func CheckAddr(addr netip.Addr) error {
	return checkAddr(addr, false)
}

// checkAddr is CheckAddr with the one exception a test may ask for.
func checkAddr(addr netip.Addr, loopbackAllowed bool) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: it is not an address", ErrForbiddenAddress)
	}

	// A zone names an interface on this machine. An address that needs one is
	// link-local by definition, and one that carries one without needing it
	// is somebody choosing which of stelfin's interfaces to leave by.
	if addr.Zone() != "" {
		return fmt.Errorf("%w: %s names a network interface", ErrForbiddenAddress, addr)
	}

	// ::ffff:127.0.0.1 is 127.0.0.1 to the kernel. It is checked as the address
	// it carries, or every IPv4 rule here could be stepped around by writing
	// the same address in its other spelling.
	addr = addr.Unmap()

	if loopbackAllowed && addr.IsLoopback() {
		return nil
	}

	if addr.Is4() {
		for _, r := range refused4 {
			if r.prefix.Contains(addr) {
				return fmt.Errorf("%w: %s — %s", ErrForbiddenAddress, addr, r.why)
			}
		}
		return nil
	}

	for _, r := range refused6 {
		if r.prefix.Contains(addr) {
			return fmt.Errorf("%w: %s — %s", ErrForbiddenAddress, addr, r.why)
		}
	}
	if !global6.Contains(addr) {
		return fmt.Errorf("%w: %s — it is outside the public IPv6 range",
			ErrForbiddenAddress, addr)
	}
	return nil
}
