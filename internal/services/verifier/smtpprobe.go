// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
)

// maxProbeMX is how many MX hosts ProbeDomain tries, in priority order.
const maxProbeMX = 2

// ProbeOptions configures a Prober.
type ProbeOptions struct {
	// HeloName is the name announced in EHLO/HELO.
	HeloName string
	// MailFrom is the envelope sender used for MAIL FROM.
	MailFrom string
	// Timeout bounds the dial and every SMTP command; a whole session is
	// bounded by three times this value.
	Timeout time.Duration
	// PerHost caps concurrent sessions to one MX host.
	PerHost int
	// MaxRcptPerSession caps RCPT commands per mail transaction; the prober
	// issues RSET and a new MAIL FROM when it is reached.
	MaxRcptPerSession int
	// Port is the SMTP port to dial (default "25").
	Port string
}

// Prober checks mailbox existence with SMTP RCPT TO without ever sending
// DATA. It is safe for concurrent use.
type Prober struct {
	opts ProbeOptions
	sems sync.Map // host -> chan struct{} (capacity PerHost)
}

// NewProber returns a Prober with defaults applied to unset options.
func NewProber(opts ProbeOptions) *Prober {
	if opts.Port == "" {
		opts.Port = "25"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	if opts.PerHost <= 0 {
		opts.PerHost = 2
	}
	if opts.MaxRcptPerSession <= 0 {
		opts.MaxRcptPerSession = 20
	}
	return &Prober{opts: opts}
}

// hostSem returns the per-host semaphore, creating it on first use.
func (p *Prober) hostSem(host string) chan struct{} {
	if v, ok := p.sems.Load(host); ok {
		return v.(chan struct{})
	}
	v, _ := p.sems.LoadOrStore(host, make(chan struct{}, p.opts.PerHost))
	return v.(chan struct{})
}

// ProbeDomain probes every address in emails (all belonging to domain) against
// the first MX hosts of mxHosts. It returns a verdict per address and whether
// the domain accepts any recipient (catch-all). When no MX can be reached,
// every address is SMTPUnknown.
func (p *Prober) ProbeDomain(ctx context.Context, domain string, mxHosts, emails []string) (map[string]SMTPVerdict, bool) {
	unknown := make(map[string]SMTPVerdict, len(emails))
	for _, e := range emails {
		unknown[e] = SMTPUnknown
	}
	if len(emails) == 0 {
		return unknown, false
	}
	hosts := mxHosts
	if len(hosts) > maxProbeMX {
		hosts = hosts[:maxProbeMX]
	}
	for _, h := range hosts {
		h = strings.TrimSuffix(h, ".")
		if h == "" {
			continue
		}
		if res, catchAll, ok := p.probeHost(ctx, h, domain, emails); ok {
			return res, catchAll
		}
		if ctx.Err() != nil {
			break
		}
	}
	return unknown, false
}

// probeHost runs one SMTP session against host. ok is false when no session
// could be established (semaphore wait cancelled, dial, greeting, EHLO or
// MAIL FROM failed), so the caller may try the next MX.
func (p *Prober) probeHost(ctx context.Context, host, domain string, emails []string) (res map[string]SMTPVerdict, catchAll, ok bool) {
	sem := p.hostSem(host)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, false, false
	}
	defer func() { <-sem }() // released after the connection is closed below

	d := &net.Dialer{Timeout: p.opts.Timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, p.opts.Port))
	if err != nil {
		return nil, false, false
	}
	// go-smtp resets the connection deadline around every command, so the
	// whole-session bound (and ctx cancellation) is enforced by closing the
	// connection instead; the deadline covers the time before the first command.
	_ = conn.SetDeadline(time.Now().Add(p.opts.Timeout * 3))
	sessCtx, cancel := context.WithTimeout(ctx, p.opts.Timeout*3)
	defer cancel()
	stop := context.AfterFunc(sessCtx, func() { _ = conn.Close() })
	defer stop()

	c := smtp.NewClient(conn)
	c.CommandTimeout = p.opts.Timeout
	c.SubmissionTimeout = p.opts.Timeout
	defer func() {
		_ = c.Quit()
		_ = c.Close()
	}()

	if err := c.Hello(p.opts.HeloName); err != nil {
		return nil, false, false
	}
	if err := c.Mail(p.opts.MailFrom, nil); err != nil {
		return nil, false, false
	}

	res = make(map[string]SMTPVerdict, len(emails))
	for _, e := range emails {
		res[e] = SMTPUnknown
	}

	// Catch-all detection: a random local part no real mailbox has.
	err = c.Rcpt("posta-probe-"+randomHex(8)+"@"+domain, nil)
	if err == nil {
		for _, e := range emails {
			res[e] = SMTPAcceptAll
		}
		return res, true, true
	}
	var se *smtp.SMTPError
	if !errors.As(err, &se) || se.Code/100 != 5 {
		// 4xx (greylisting, rate limiting) or a broken session: nothing
		// said about the real addresses can be trusted.
		return res, false, true
	}

	inTx := 1 // the catch-all probe counts towards the transaction limit
	for _, e := range emails {
		if inTx >= p.opts.MaxRcptPerSession {
			if c.Reset() != nil || c.Mail(p.opts.MailFrom, nil) != nil {
				break // remaining addresses stay SMTPUnknown
			}
			inTx = 0
		}
		err := c.Rcpt(e, nil)
		inTx++
		res[e] = classifyRcpt(err)
		if err != nil && !errors.As(err, &se) {
			break // connection-level failure: the session is gone
		}
	}
	return res, false, true
}

// classifyRcpt maps an RCPT TO reply to a verdict. Only replies that clearly
// say the recipient mailbox does not exist are Undeliverable. Precedence:
//  1. nil is Deliverable; an error that is not *smtp.SMTPError is Unknown.
//  2. Undeliverable always requires a permanent (5xx) basic code.
//  3. When an enhanced code is present it decides: 5.1.x is Undeliverable,
//     except 5.1.7/5.1.8 (bad sender, reported at RCPT with delayed reject),
//     which are Unknown; any other class/subject (5.7.x policy, 5.2.x
//     mailbox full, ...) is Unknown whatever the basic code says.
//  4. Only without an enhanced code does the basic code decide: 550, 551
//     and 553 are Undeliverable, everything else is Unknown.
func classifyRcpt(err error) SMTPVerdict {
	if err == nil {
		return SMTPDeliverable
	}
	var se *smtp.SMTPError
	if !errors.As(err, &se) {
		return SMTPUnknown
	}
	if se.Code/100 != 5 {
		return SMTPUnknown
	}
	// go-smtp leaves EnhancedCode all zeros when the reply carried none.
	if ec := se.EnhancedCode; ec[0] > 0 {
		if ec[0] == 5 && ec[1] == 1 && ec[2] != 7 && ec[2] != 8 {
			return SMTPUndeliverable
		}
		return SMTPUnknown
	}
	switch se.Code {
	case 550, 551, 553:
		return SMTPUndeliverable
	}
	return SMTPUnknown
}

// randomHex returns 2*n lowercase hex characters from crypto/rand.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return hex.EncodeToString(b)
}
