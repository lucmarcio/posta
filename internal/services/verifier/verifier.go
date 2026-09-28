// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package verifier checks whether an email address is valid/deliverable without
// sending to it. It runs cheap-to-expensive checks (syntax, suppression/bounce
// history, disposable/role detection, MX lookup) and caches the intrinsic result
// in Redis so the same address and domain are not re-checked on every call.
//
// Note: the MVP does not perform an SMTP RCPT probe, so mailbox existence is not
// confirmed; checks.smtp is reported as "skipped".
package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"time"

	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/redis/go-redis/v9"
)

// ErrRateLimited is returned by Verify when the per-user hourly limit is hit.
var ErrRateLimited = errors.New("verification rate limit exceeded")

// Status is the overall verdict for an address.
type Status string

const (
	StatusValid      Status = "valid"      // syntax ok + domain accepts mail (mailbox not probed)
	StatusInvalid    Status = "invalid"    // definitely undeliverable
	StatusRisky      Status = "risky"      // deliverable but discouraged (role account)
	StatusDisposable Status = "disposable" // throwaway provider
	StatusUnknown    Status = "unknown"    // could not determine (e.g. DNS error)
	StatusAcceptAll  Status = "accept_all" // domain accepts all addresses (catch-all)
)

// SMTPVerdict is the outcome of an SMTP RCPT probe (not performed in the MVP).
type SMTPVerdict string

const (
	SMTPSkipped       SMTPVerdict = "skipped"
	SMTPDeliverable   SMTPVerdict = "deliverable"
	SMTPUndeliverable SMTPVerdict = "undeliverable"
	SMTPAcceptAll     SMTPVerdict = "accept_all"
	SMTPUnknown       SMTPVerdict = "unknown"
)

// Checks records which individual checks passed.
type Checks struct {
	Syntax      bool        `json:"syntax"`
	MX          bool        `json:"mx"`
	Disposable  bool        `json:"disposable"`
	RoleAccount bool        `json:"role_account"`
	SMTP        SMTPVerdict `json:"smtp"` // always "skipped"; no SMTP probe is performed
}

// Result is the verification outcome returned to the caller. The intrinsic
// fields (everything except Suppressed/PreviouslyBounced/Cached) are what gets
// cached in Redis; the per-tenant flags are layered on per request.
type Result struct {
	Email             string    `json:"email"`
	Status            Status    `json:"status" enum:"valid,invalid,risky,accept_all,disposable,unknown"`
	Score             int       `json:"score"`
	Checks            Checks    `json:"checks"`
	Reason            string    `json:"reason,omitempty"`
	Suggestion        string    `json:"suggestion,omitempty"`
	MailboxVerified   bool      `json:"mailbox_verified"`
	Suppressed        bool      `json:"suppressed"`
	PreviouslyBounced bool      `json:"previously_bounced"`
	Cached            bool      `json:"cached"`
	CheckedAt         time.Time `json:"checked_at"`
}

// Options configures a Service. Durations and limits come from app config.
type Options struct {
	Enabled    bool
	AddrTTL    time.Duration
	MXTTL      time.Duration
	RateHourly int // per-user hourly cap; 0 disables rate limiting
}

// Service verifies email addresses and caches results in Redis.
type Service struct {
	client          *redis.Client
	suppressionRepo *repositories.SuppressionRepository
	bounceRepo      *repositories.BounceRepository
	opts            Options
	resolver        Resolver // injectable for tests; nil uses the default
}

// Resolver is the DNS surface the verifier needs; *net.Resolver satisfies it.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// SetResolver swaps the DNS resolver (tests, custom resolvers).
func (s *Service) SetResolver(r Resolver) { s.resolver = r }

// NewService builds a verifier. suppressionRepo/bounceRepo may be nil (the
// per-tenant overlay is then skipped); client must be non-nil for caching.
func NewService(client *redis.Client, suppressionRepo *repositories.SuppressionRepository, bounceRepo *repositories.BounceRepository, opts Options) *Service {
	return &Service{
		client:          client,
		suppressionRepo: suppressionRepo,
		bounceRepo:      bounceRepo,
		opts:            opts,
		resolver:        net.DefaultResolver,
	}
}

// Enabled reports whether verification is turned on.
func (s *Service) Enabled() bool { return s.opts.Enabled }

// Verify checks an address. fresh=true bypasses the Redis cache. The returned
// Result always reflects the current tenant's suppression/bounce history.
func (s *Service) Verify(ctx context.Context, scope repositories.ResourceScope, rawEmail string, fresh bool) (*Result, error) {
	email := strings.ToLower(strings.TrimSpace(rawEmail))

	// Syntax. A malformed address is conclusively invalid.
	addr, err := mail.ParseAddress(email)
	if err != nil {
		r := baseResult(email)
		r.Status = StatusInvalid
		r.Reason = "invalid syntax"
		return r, nil
	}
	email = strings.ToLower(addr.Address)

	// Per-user rate limit (fail-open on Redis errors).
	if s.opts.RateHourly > 0 {
		if err := s.checkRate(ctx, scope.UserID); err != nil {
			return nil, err
		}
	}

	// Per-tenant history overlay (no network). Suppressed or previously
	//    hard-bounced addresses are conclusively bad for this tenant.
	suppressed, bounced := s.tenantOverlay(scope, email)
	if suppressed || bounced {
		r := baseResult(email)
		r.Checks.Syntax = true
		r.Status = StatusInvalid
		r.Score = 0
		r.Suppressed = suppressed
		r.PreviouslyBounced = bounced
		if suppressed {
			r.Reason = "address is on the suppression list"
		} else {
			r.Reason = "address previously hard-bounced"
		}
		return r, nil
	}

	// Redis cache for the intrinsic result.
	if !fresh {
		if cached := s.getCache(ctx, email); cached != nil {
			cached.Cached = true
			cached.Suppressed = suppressed
			cached.PreviouslyBounced = bounced
			return cached, nil
		}
	}

	// Compute the intrinsic result (disposable/role/MX) and cache it.
	r := s.compute(ctx, email)
	s.setCache(ctx, email, r)

	r.Suppressed = suppressed
	r.PreviouslyBounced = bounced
	return r, nil
}

// compute runs the intrinsic checks for a syntactically valid address.
func (s *Service) compute(ctx context.Context, email string) *Result {
	r := baseResult(email)
	r.Checks.Syntax = true

	at := strings.LastIndex(email, "@")
	local, domain := email[:at], email[at+1:]

	if sug := suggestDomain(domain); sug != "" {
		r.Suggestion = local + "@" + sug
	}

	disposable := isDisposable(domain)
	role := isRoleAccount(local)
	r.Checks.Disposable = disposable
	r.Checks.RoleAccount = role

	// Disposable domains are disqualified without a DNS lookup.
	if disposable {
		r.Status, r.Score, r.Reason = decide(verdictInput{SyntaxOK: true, Disposable: true, Role: role, DNS: dnsOK, SMTP: SMTPSkipped})
		return r
	}

	_, outcome, nullMX := s.mxHosts(ctx, domain)
	r.Checks.MX = !disposable && outcome == dnsOK
	r.Status, r.Score, r.Reason = decide(verdictInput{SyntaxOK: true, Disposable: disposable, Role: role, DNS: outcome, NullMX: nullMX, SMTP: SMTPSkipped})
	return r
}

// dnsOutcome is the conclusiveness of a domain's mail-acceptance lookup.
type dnsOutcome int

const (
	dnsOK      dnsOutcome = iota // domain can receive mail
	dnsNoMail                    // NXDOMAIN, no MX/A, or null MX: conclusively cannot
	dnsTempErr                   // timeout/SERVFAIL: undetermined, never cached
)

// verdictInput is the pure input to decide, gathering every signal that
// contributes to the final verdict.
type verdictInput struct {
	SyntaxOK, Disposable, Role bool
	DNS                        dnsOutcome
	NullMX                     bool
	SMTP                       SMTPVerdict
}

// decide encodes the verdict precedence as a pure function so it is easy to test.
func decide(in verdictInput) (Status, int, string) {
	switch {
	case !in.SyntaxOK:
		return StatusInvalid, 0, "invalid syntax"
	case in.Disposable:
		return StatusDisposable, 10, "disposable email provider"
	case in.DNS == dnsTempErr:
		return StatusUnknown, 40, "DNS lookup failed temporarily"
	case in.DNS == dnsNoMail && in.NullMX:
		return StatusInvalid, 0, "domain does not accept mail (null MX)"
	case in.DNS == dnsNoMail:
		return StatusInvalid, 0, "domain has no mail exchanger (MX/A) records"
	case in.SMTP == SMTPUndeliverable:
		return StatusInvalid, 0, "mailbox does not exist"
	case in.Role:
		return StatusRisky, 60, "role-based address"
	case in.SMTP == SMTPAcceptAll:
		return StatusAcceptAll, 50, "domain accepts all addresses (catch-all)"
	case in.SMTP == SMTPDeliverable:
		return StatusValid, 95, ""
	default:
		return StatusValid, 90, ""
	}
}

// shouldCache keeps undetermined results out of the address cache.
func shouldCache(r *Result) bool { return r.Status != StatusUnknown }

// tenantOverlay returns the current tenant's suppression/bounce status.
func (s *Service) tenantOverlay(scope repositories.ResourceScope, email string) (suppressed, bounced bool) {
	if s.suppressionRepo != nil {
		if ok, err := s.suppressionRepo.IsSuppressed(scope, email); err == nil {
			suppressed = ok
		}
	}
	if s.bounceRepo != nil {
		if n, err := s.bounceRepo.CountHardBouncesByRecipient(scope.UserID, email); err == nil && n > 0 {
			bounced = true
		}
	}
	return suppressed, bounced
}

const nullMXSentinel = "nullmx"

// mxHosts returns the domain's mail hosts and the DNS outcome, caching only
// conclusive answers ("none"/"nullmx" sentinels for negatives).
func (s *Service) mxHosts(ctx context.Context, domain string) ([]string, dnsOutcome, bool) {
	key := mxKey(domain)
	if s.client != nil {
		if v, err := s.client.Get(ctx, key).Result(); err == nil {
			switch v {
			case "", "none":
				return nil, dnsNoMail, false
			case nullMXSentinel:
				return nil, dnsNoMail, true
			default:
				return strings.Split(v, ","), dnsOK, false
			}
		}
	}

	hosts, outcome, nullMX := s.lookupMX(ctx, domain)

	if s.client != nil && outcome != dnsTempErr {
		val := strings.Join(hosts, ",")
		if outcome == dnsNoMail {
			val = "none"
			if nullMX {
				val = nullMXSentinel
			}
		}
		s.client.Set(ctx, key, val, s.opts.MXTTL)
	}
	return hosts, outcome, nullMX
}

func (s *Service) lookupMX(ctx context.Context, domain string) ([]string, dnsOutcome, bool) {
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resolver := s.resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	mxs, err := resolver.LookupMX(lookupCtx, domain)
	switch {
	case err == nil && len(mxs) > 0:
		// RFC 7505: a single "." exchanger means the domain accepts no mail.
		if len(mxs) == 1 && strings.TrimSuffix(mxs[0].Host, ".") == "" {
			return nil, dnsNoMail, true
		}
		hosts := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			hosts = append(hosts, mx.Host)
		}
		return hosts, dnsOK, false
	case err != nil && !isNotFound(err):
		return nil, dnsTempErr, false
	}

	// No MX: a domain with an A/AAAA record still receives mail (RFC 5321 §5.1).
	addrs, err := resolver.LookupHost(lookupCtx, domain)
	switch {
	case err == nil && len(addrs) > 0:
		return []string{domain}, dnsOK, false
	case err != nil && !isNotFound(err):
		return nil, dnsTempErr, false
	}
	return nil, dnsNoMail, false
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// checkRate increments a per-user hourly counter; fails open on Redis errors.
func (s *Service) checkRate(ctx context.Context, userID uint) error {
	if s.client == nil {
		return nil
	}
	key := fmt.Sprintf("verify:rl:hour:%d:%s", userID, time.Now().Format("2006010215"))
	n, err := s.client.Incr(ctx, key).Result()
	if err != nil {
		return nil
	}
	if n == 1 {
		s.client.Expire(ctx, key, time.Hour)
	}
	if n > int64(s.opts.RateHourly) {
		return ErrRateLimited
	}
	return nil
}

func (s *Service) getCache(ctx context.Context, email string) *Result {
	if s.client == nil {
		return nil
	}
	val, err := s.client.Get(ctx, addrKey(email)).Result()
	if err != nil {
		return nil
	}
	var r Result
	if json.Unmarshal([]byte(val), &r) != nil {
		return nil
	}
	return &r
}

func (s *Service) setCache(ctx context.Context, email string, r *Result) {
	if s.client == nil || !shouldCache(r) {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	s.client.Set(ctx, addrKey(email), b, s.opts.AddrTTL)
}

func baseResult(email string) *Result {
	return &Result{
		Email:     email,
		Status:    StatusUnknown,
		Checks:    Checks{SMTP: SMTPSkipped},
		CheckedAt: time.Now().UTC(),
	}
}

func addrKey(email string) string { return "verify:addr:" + email }
func mxKey(domain string) string  { return "verify:mx:" + strings.ToLower(domain) }
