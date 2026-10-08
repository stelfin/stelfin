package egress

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// policy is what one client permits.
type policy struct {
	// ports are the ports that may be dialled. 443 is always among them.
	ports map[uint16]bool
	// relaxed permits loopback and any port. Set only through ForTesting.
	relaxed bool
}

func (p policy) portAllowed(port uint16) bool {
	return p.relaxed || p.ports[port]
}

// ValidateURL refuses a URL that could never be fetched.
//
// For registration: when an admin types a connector's address, this is what
// tells them at once that http is not accepted or that 10.0.0.5 is not
// somewhere stelfin goes, rather than letting the mistake surface as a failed
// read a week later.
//
// It is syntactic and resolves nothing, and that is the reason it is not the
// security check. A name that passes here says nothing about where the name
// points, and where it points is the attacker's to change after registration
// and again between any two requests. The check that counts is the one made at
// dial time, on the resolved address, by the client. This one exists so that
// honest mistakes are caught early and so that the URL shapes with no
// legitimate use never get as far as a resolver.
//
// It applies the strictest policy: port 443 only. A deployment that has allowed
// other ports asks its Client instead.
func ValidateURL(raw string) error {
	return checkRawURL(raw, policy{ports: map[uint16]bool{defaultPort: true}})
}

func checkRawURL(raw string, p policy) error {
	if len(raw) > maxURLLength {
		return fmt.Errorf("%w: it is longer than %d bytes", ErrBadURL, maxURLLength)
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse quotes its input in the error, and a URL being registered
		// may carry a secret in its query. Said plainly instead.
		return fmt.Errorf("%w: it cannot be parsed", ErrBadURL)
	}
	return checkURL(u, p)
}

// maxURLLength bounds a URL. Nothing legitimate is near it.
const maxURLLength = 2048

// defaultPort is the one port permitted without being asked for.
const defaultPort uint16 = 443

// checkURL is the syntactic check, applied at registration and again to every
// request on its way out.
func checkURL(u *url.URL, p policy) error {
	// https only. A connector's response is read as data about a treasury, and
	// over plain http anybody on the path writes that data — and reads the
	// bearer credential on its way past.
	if u.Scheme != "https" {
		return fmt.Errorf("%w: the scheme must be https", ErrBadURL)
	}
	if u.Opaque != "" {
		return fmt.Errorf("%w: it has no host", ErrBadURL)
	}

	// https://sheets.googleapis.com@203.0.113.9/ goes to 203.0.113.9. Userinfo
	// has no use here — the credential travels in a header — and its only
	// remaining function is to make a URL look as though it goes somewhere it
	// does not.
	if u.User != nil {
		return fmt.Errorf("%w: it must not carry a username or password", ErrBadURL)
	}

	port := defaultPort
	if text := u.Port(); text != "" {
		n, err := strconv.ParseUint(text, 10, 16)
		if err != nil || n == 0 {
			return fmt.Errorf("%w: the port is not a port", ErrBadURL)
		}
		port = uint16(n)
	}
	// The port is restricted because an address being public does not make
	// every service on it a web service. A request to port 25 or 6379 is a
	// request whose first line an attacker partly controls, sent to something
	// that was never meant to parse HTTP.
	if !p.portAllowed(port) {
		return fmt.Errorf("%w: port %d is not permitted", ErrBadURL, port)
	}

	return checkHost(u.Hostname(), p)
}

// checkHost refuses the hosts that can be refused without resolving them.
func checkHost(host string, p policy) error {
	if host == "" {
		return fmt.Errorf("%w: it has no host", ErrBadURL)
	}

	// An address written as an address is checked as one, here and now.
	//
	// What counts as "written as an address" is decided by net/netip, and it
	// is strict: dotted decimal with exactly four fields and no leading zeros,
	// or RFC 4291 IPv6. It does not read 0177.0.0.1 as octal, 0x7f.0.0.1 as
	// hex, 2130706433 as a 32-bit integer or 127.1 as shorthand — all of which
	// the C library's inet_aton does, and all of which mean 127.0.0.1 to it.
	// So those spellings are not addresses to this process; they fall through
	// to the hostname rules below, which refuse them by shape, and if one ever
	// reached a resolver that did understand it, the answer would come back as
	// 127.0.0.1 and be refused by the dial-time check like any other.
	if addr, err := netip.ParseAddr(host); err == nil {
		if err := checkAddr(addr, p.relaxed); err != nil {
			return fmt.Errorf("%w: %w", ErrBadURL, err)
		}
		return nil
	}

	if len(host) > 253 {
		return fmt.Errorf("%w: the host name is too long", ErrBadURL)
	}
	labels := strings.Split(strings.ToLower(host), ".")

	// A single label — "localhost", "metadata", "postgres" — is completed from
	// the machine's search domains, which makes it a name inside whatever
	// network stelfin runs in. No public service is addressed that way.
	if len(labels) < 2 {
		return fmt.Errorf("%w: %q is not a public host name", ErrBadURL, host)
	}
	for _, label := range labels {
		// An empty label is a doubled or trailing dot. "example.com." is the
		// same host as "example.com" to a resolver and a different string to
		// everything that compares hosts, and one registered URL should have
		// one spelling.
		if !validLabel(label) {
			return fmt.Errorf("%w: %q is not a host name", ErrBadURL, host)
		}
	}

	// A name whose last label is a number is not a name: no top-level domain
	// is numeric, so the only thing that would resolve it is a library reading
	// it as one of inet_aton's address spellings.
	if last := labels[len(labels)-1]; numeric(last) {
		return fmt.Errorf("%w: %q is an address in disguise", ErrBadURL, host)
	}
	return nil
}

// validLabel reports whether s is one DNS label in letters, digits and hyphens.
//
// ASCII only. An internationalised name is registered in its punycode form,
// because two names that render identically and resolve differently is a
// confusion this has no way to settle.
func validLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// numeric reports whether a label is decimal, or hex with a 0x prefix.
func numeric(label string) bool {
	if rest, found := strings.CutPrefix(label, "0x"); found {
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(label); i++ {
		if label[i] < '0' || label[i] > '9' {
			return false
		}
	}
	return true
}
