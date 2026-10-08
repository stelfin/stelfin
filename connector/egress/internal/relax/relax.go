// Package relax holds the one type that can loosen an egress client.
//
// It exists to be unreachable. Go refuses an import of an internal package from
// outside the directory that contains it, so only code under connector/egress
// can name Token — and a value nobody can name is a value nobody can pass. That
// is what makes "allow loopback" a thing a test can ask for and a deployment
// cannot: there is no environment variable, no option and no struct field to
// set, because the argument that would carry the request cannot be written
// down anywhere a configuration is read.
package relax

import "crypto/x509"

// Token is permission to reach a loopback test server.
type Token struct {
	// Roots are the certificates of the test server, trusted in addition to
	// nothing: a relaxed client believes this pool and not the system's.
	Roots *x509.CertPool
}
