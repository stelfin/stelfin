// Package egresstest lets a test reach an httptest server through egress.
//
// Everything egress refuses is what a test server is: a loopback address, on a
// port that is not 443, presenting a certificate no public authority signed.
// The connectors still have to be tested through the real client, because a
// test that swaps in a plain http.Client is a test of some other code path — so
// this package relaxes exactly those three things and no others.
//
// It is for tests only, and it is arranged so that this is more than a request.
// The function takes a testing.TB and an httptest.Server, so calling it means
// importing the testing packages into a binary, and the relaxation itself is
// carried by a type in an internal package that nothing outside connector/egress
// can name. No configuration value reaches it.
package egresstest

import (
	"crypto/x509"
	"net/http/httptest"
	"testing"

	"github.com/stelfin/stelfin/connector/egress"
	"github.com/stelfin/stelfin/connector/egress/internal/relax"
)

// Client returns an egress client that can reach one TLS test server.
//
// What is relaxed: loopback addresses are permitted, any port is permitted, and
// the server's own certificate is trusted. What is not: the scheme is still
// https, redirects are still refused, userinfo is still refused, every
// non-loopback private range is still refused, and the address dialled is still
// the one that was checked. A plain httptest.NewServer is refused here for that
// reason — a test that needed http would be testing a client that does not
// exist in production.
func Client(tb testing.TB, srv *httptest.Server, options egress.Options) *egress.Client {
	tb.Helper()
	if srv == nil || srv.TLS == nil || srv.Certificate() == nil {
		tb.Fatal("egresstest: the server must be started with httptest.NewTLSServer")
	}

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return egress.NewClient(egress.ForTesting(options, relax.Token{Roots: roots}))
}
