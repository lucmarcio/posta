// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"context"
	"net"
	"testing"
)

type fakeResolver struct {
	mx      map[string][]*net.MX
	mxErr   map[string]error
	host    map[string][]string
	hostErr map[string]error
}

func (f *fakeResolver) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	return f.mx[d], f.mxErr[d]
}
func (f *fakeResolver) LookupHost(_ context.Context, d string) ([]string, error) {
	return f.host[d], f.hostErr[d]
}

var errNotFound = &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}
var errTimeout = &net.DNSError{Err: "i/o timeout", Name: "x", IsTimeout: true, IsTemporary: true}

func newTestService(r Resolver) *Service {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(r)
	return s
}

func TestNullMXIsInvalid(t *testing.T) {
	s := newTestService(&fakeResolver{mx: map[string][]*net.MX{"example.com": {{Host: ".", Pref: 0}}}})
	r := s.compute(context.Background(), "a@example.com")
	if r.Status != StatusInvalid || r.Checks.MX || r.Reason != "domain does not accept mail (null MX)" {
		t.Fatalf("got %+v", r)
	}
}

func TestDNSTimeoutIsUnknownAndNotCached(t *testing.T) {
	s := newTestService(&fakeResolver{mxErr: map[string]error{"slow.com": errTimeout}})
	r := s.compute(context.Background(), "a@slow.com")
	if r.Status != StatusUnknown {
		t.Fatalf("status = %s, want unknown", r.Status)
	}
	if shouldCache(r) {
		t.Fatal("unknown results must not be cached")
	}
}

func TestNXDomainIsInvalid(t *testing.T) {
	s := newTestService(&fakeResolver{
		mxErr:   map[string]error{"nope.invalid": errNotFound},
		hostErr: map[string]error{"nope.invalid": errNotFound},
	})
	if r := s.compute(context.Background(), "a@nope.invalid"); r.Status != StatusInvalid {
		t.Fatalf("status = %s", r.Status)
	}
}

func TestARecordFallbackIsValid(t *testing.T) {
	s := newTestService(&fakeResolver{
		mxErr: map[string]error{"a-only.com": errNotFound},
		host:  map[string][]string{"a-only.com": {"192.0.2.1"}},
	})
	if r := s.compute(context.Background(), "a@a-only.com"); r.Status != StatusValid || !r.Checks.MX {
		t.Fatalf("got %+v", r)
	}
}

func TestHostTimeoutAfterMXNotFoundIsUnknown(t *testing.T) {
	s := newTestService(&fakeResolver{
		mxErr:   map[string]error{"flaky.com": errNotFound},
		hostErr: map[string]error{"flaky.com": errTimeout},
	})
	if r := s.compute(context.Background(), "a@flaky.com"); r.Status != StatusUnknown {
		t.Fatalf("status = %s", r.Status)
	}
}
