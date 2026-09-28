// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type probeBackend struct {
	known    map[string]bool
	catchAll bool
	greylist bool

	mu          sync.Mutex
	mails       int
	dataCalls   int
	maxPerTx    int
	probeRcpts  []string
	sessionOpen int
}

func (b *probeBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	b.mu.Lock()
	b.sessionOpen++
	b.mu.Unlock()
	return &probeSession{b: b}, nil
}

type probeSession struct {
	b    *probeBackend
	inTx int
}

func (s *probeSession) Mail(string, *smtp.MailOptions) error {
	s.b.mu.Lock()
	s.b.mails++
	s.b.mu.Unlock()
	s.inTx = 0
	return nil
}

func (s *probeSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.inTx++
	s.b.mu.Lock()
	if s.inTx > s.b.maxPerTx {
		s.b.maxPerTx = s.inTx
	}
	if strings.HasPrefix(to, "posta-probe-") {
		s.b.probeRcpts = append(s.b.probeRcpts, to)
	}
	s.b.mu.Unlock()
	switch {
	case s.b.greylist:
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "greylisted"}
	case s.b.catchAll || s.b.known[strings.ToLower(to)]:
		return nil
	default:
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	}
}

func (s *probeSession) Data(io.Reader) error {
	s.b.mu.Lock()
	s.b.dataCalls++
	s.b.mu.Unlock()
	return nil
}
func (s *probeSession) Reset()        { s.inTx = 0 }
func (s *probeSession) Logout() error { return nil }

func startProbeServer(t *testing.T, be *probeBackend) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := smtp.NewServer(be)
	srv.Domain = "mx.test"
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func testProber(port string) *Prober {
	return NewProber(ProbeOptions{HeloName: "verify.test", MailFrom: "probe@verify.test",
		Timeout: 3 * time.Second, PerHost: 2, MaxRcptPerSession: 2, Port: port})
}

func TestProbeDeliverableAndUndeliverable(t *testing.T) {
	be := &probeBackend{known: map[string]bool{"a@d.test": true, "c@d.test": true}}
	port := startProbeServer(t, be)
	got, catchAll := testProber(port).ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"},
		[]string{"a@d.test", "b@d.test", "c@d.test"}) // 3 > MaxRcptPerSession: exercises RSET
	if catchAll {
		t.Fatal("not a catch-all")
	}
	want := map[string]SMTPVerdict{"a@d.test": SMTPDeliverable, "b@d.test": SMTPUndeliverable, "c@d.test": SMTPDeliverable}
	for e, v := range want {
		if got[e] != v {
			t.Errorf("%s = %s, want %s", e, got[e], v)
		}
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.mails < 2 {
		t.Errorf("MAIL FROM sent %d times, want a new transaction after RSET", be.mails)
	}
	if be.maxPerTx > 2 {
		t.Errorf("a transaction carried %d RCPTs, want <= MaxRcptPerSession (2)", be.maxPerTx)
	}
	if be.dataCalls != 0 {
		t.Errorf("DATA sent %d times, must never be sent", be.dataCalls)
	}
}

func TestProbeCatchAll(t *testing.T) {
	be := &probeBackend{catchAll: true}
	port := startProbeServer(t, be)
	p := testProber(port)
	got, catchAll := p.ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test"})
	if !catchAll || got["x@d.test"] != SMTPAcceptAll {
		t.Fatalf("catchAll=%v got=%v", catchAll, got)
	}
	_, _ = p.ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test"})
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.probeRcpts) != 2 || be.probeRcpts[0] == be.probeRcpts[1] {
		t.Fatalf("probe recipients must be random per session: %v", be.probeRcpts)
	}
	local := strings.TrimSuffix(strings.TrimPrefix(be.probeRcpts[0], "posta-probe-"), "@d.test")
	if len(local) != 16 || strings.Trim(local, "0123456789abcdef") != "" {
		t.Fatalf("probe local part must be 16 hex chars, got %q", be.probeRcpts[0])
	}
}

func TestProbeGreylistIsUnknown(t *testing.T) {
	port := startProbeServer(t, &probeBackend{greylist: true})
	got, _ := testProber(port).ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test"})
	if got["x@d.test"] != SMTPUnknown {
		t.Fatalf("got %v", got)
	}
}

func TestProbeUnreachableIsUnknown(t *testing.T) {
	got, _ := testProber("1").ProbeDomain(context.Background(), "d.test", []string{"127.0.0.1"}, []string{"x@d.test", "y@d.test"})
	for _, e := range []string{"x@d.test", "y@d.test"} {
		if got[e] != SMTPUnknown {
			t.Fatalf("%s: got %v", e, got)
		}
	}
}

func TestProbeFallsBackToSecondMXAndTrimsDot(t *testing.T) {
	port := startProbeServer(t, &probeBackend{known: map[string]bool{"a@d.test": true}})
	// 127.0.0.2 is loopback but nothing listens there: connection refused.
	got, _ := testProber(port).ProbeDomain(context.Background(), "d.test", []string{"127.0.0.2", "127.0.0.1."}, []string{"a@d.test"})
	if got["a@d.test"] != SMTPDeliverable {
		t.Fatalf("got %v", got)
	}
}

func TestProbeTriesAtMostTwoMX(t *testing.T) {
	port := startProbeServer(t, &probeBackend{known: map[string]bool{"a@d.test": true}})
	got, _ := testProber(port).ProbeDomain(context.Background(), "d.test",
		[]string{"127.0.0.2", "127.0.0.3", "127.0.0.1"}, []string{"a@d.test"})
	if got["a@d.test"] != SMTPUnknown {
		t.Fatalf("third MX must not be tried, got %v", got)
	}
}

func TestProbeSemaphoreHonoursContext(t *testing.T) {
	be := &probeBackend{known: map[string]bool{"a@d.test": true}}
	port := startProbeServer(t, be)
	p := NewProber(ProbeOptions{HeloName: "verify.test", MailFrom: "probe@verify.test",
		Timeout: 3 * time.Second, PerHost: 1, Port: port})
	sem := p.hostSem("127.0.0.1")
	sem <- struct{}{} // occupy the only slot
	defer func() { <-sem }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, _ := p.ProbeDomain(ctx, "d.test", []string{"127.0.0.1"}, []string{"a@d.test"})
	if got["a@d.test"] != SMTPUnknown {
		t.Fatalf("got %v", got)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("semaphore wait ignored ctx cancellation")
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if be.sessionOpen != 0 {
		t.Fatalf("dialled while the per-host limit was exhausted")
	}
}

func TestProbeCancelledContextIsUnknown(t *testing.T) {
	port := startProbeServer(t, &probeBackend{known: map[string]bool{"a@d.test": true}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _ := testProber(port).ProbeDomain(ctx, "d.test", []string{"127.0.0.1"}, []string{"a@d.test"})
	if got["a@d.test"] != SMTPUnknown {
		t.Fatalf("got %v", got)
	}
}

func TestNewProberDefaults(t *testing.T) {
	p := NewProber(ProbeOptions{})
	if p.opts.Port != "25" || p.opts.Timeout != 10*time.Second || p.opts.PerHost != 2 || p.opts.MaxRcptPerSession != 20 {
		t.Fatalf("defaults = %+v", p.opts)
	}
}

func TestClassifyRcpt(t *testing.T) {
	e := func(code int, a, b, c int) error {
		return &smtp.SMTPError{Code: code, EnhancedCode: smtp.EnhancedCode{a, b, c}}
	}
	cases := []struct {
		err  error
		want SMTPVerdict
	}{
		{nil, SMTPDeliverable},
		{e(550, 5, 1, 1), SMTPUndeliverable},
		{e(553, 5, 1, 3), SMTPUndeliverable},
		{e(551, 0, 0, 0), SMTPUndeliverable},
		{&smtp.SMTPError{Code: 550}, SMTPUndeliverable}, // no enhanced code: basic code decides
		{e(550, 5, 7, 1), SMTPUnknown},                  // policy block, not a missing mailbox
		{e(553, 5, 7, 1), SMTPUnknown},
		{e(550, 5, 1, 8), SMTPUnknown}, // bad sender domain reported at RCPT
		{e(550, 5, 1, 7), SMTPUnknown}, // bad sender mailbox syntax
		{e(450, 5, 1, 1), SMTPUnknown}, // temporary basic code wins
		{e(530, 0, 0, 0), SMTPUnknown},
		{e(501, 5, 1, 3), SMTPUndeliverable},
		{e(552, 5, 2, 2), SMTPUnknown},
		{e(554, 5, 7, 1), SMTPUnknown},
		{e(450, 4, 2, 1), SMTPUnknown},
		{errors.New("broken pipe"), SMTPUnknown},
	}
	for _, c := range cases {
		if got := classifyRcpt(c.err); got != c.want {
			t.Errorf("classifyRcpt(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
