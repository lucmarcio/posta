// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package verifier checks whether an email address is valid/deliverable without
// sending to it. It runs cheap-to-expensive checks (syntax, suppression/bounce
// history, disposable/role detection, MX lookup) and caches the intrinsic result
// in Redis so the same address and domain are not re-checked on every call.
//
// Note: SMTP RCPT probing is optional and disabled by default
// (POSTA_EMAIL_VERIFY_SMTP_ENABLED). When off, mailbox existence is not
// confirmed and checks.smtp is reported as "skipped". When on, addresses of
// domains with a conclusive MX are probed and checks.smtp reports the RCPT
// outcome ("deliverable", "undeliverable", "accept_all" for catch-all
// domains, or "unknown" when the probe could not determine an answer).
package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/jkaninda/logger"
	"github.com/redis/go-redis/v9"
)

// ErrRateLimited is returned by Verify/VerifyMany when the per-user hourly limit is hit.
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

// SMTPVerdict is the outcome of the optional SMTP RCPT probe; it is
// SMTPSkipped when probing is disabled (the default) or not attempted.
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
	SMTP        SMTPVerdict `json:"smtp"` // "skipped" unless SMTP probing is enabled; see package doc
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
	Enabled     bool
	AddrTTL     time.Duration
	MXTTL       time.Duration
	RateHourly  int // per-user hourly cap; 0 disables rate limiting
	Concurrency int // max domains resolved in parallel by VerifyMany; <= 0 uses 16

	// SMTPEnabled turns on the optional SMTP RCPT probe; off by default.
	SMTPEnabled bool
	// SMTP configures the prober NewService creates when SMTPEnabled is true.
	SMTP ProbeOptions
}

// domainProber is the SMTP-probing surface computeDomain needs; *Prober
// satisfies it. Kept as an interface so tests can inject a fake.
type domainProber interface {
	ProbeDomain(ctx context.Context, domain string, mxHosts, emails []string) (map[string]SMTPVerdict, bool)
}

// Service verifies email addresses and caches results in Redis.
type Service struct {
	client       *redis.Client
	suppressions suppressionLookup
	bounces      bounceLookup
	opts         Options
	resolver     Resolver     // injectable for tests; nil uses the default
	prober       domainProber // injectable for tests; nil disables SMTP probing regardless of opts
}

// SetProber swaps the SMTP prober (tests, custom probers).
func (s *Service) SetProber(p domainProber) { s.prober = p }

// suppressionLookup is the batched suppression check the overlay needs.
type suppressionLookup interface {
	SuppressedSet(scope repositories.ResourceScope, emails []string) (map[string]struct{}, error)
}

// bounceLookup is the batched hard-bounce check the overlay needs.
type bounceLookup interface {
	HardBouncedSet(scope repositories.ResourceScope, emails []string) (map[string]struct{}, error)
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
	s := &Service{
		client:   client,
		opts:     opts,
		resolver: net.DefaultResolver,
	}
	// Assign only non-nil pointers: a nil pointer stored in an interface
	// would make the interface itself non-nil.
	if suppressionRepo != nil {
		s.suppressions = suppressionRepo
	}
	if bounceRepo != nil {
		s.bounces = bounceRepo
	}
	if opts.SMTPEnabled {
		s.prober = NewProber(opts.SMTP)
	}
	return s
}

// Enabled reports whether verification is turned on.
func (s *Service) Enabled() bool { return s.opts.Enabled }

// Verify checks one address; it is VerifyMany with a single input. fresh=true
// bypasses the Redis cache. The returned Result always reflects the current
// tenant's suppression/bounce history.
func (s *Service) Verify(ctx context.Context, scope repositories.ResourceScope, rawEmail string, fresh bool) (*Result, error) {
	res, err := s.VerifyMany(ctx, scope, []string{rawEmail}, fresh)
	if err != nil {
		return nil, err
	}
	return res[0], nil
}

// VerifyMany verifies addresses in input order, charging the hourly rate
// limit once per distinct syntactically valid address.
func (s *Service) VerifyMany(ctx context.Context, scope repositories.ResourceScope, emails []string, fresh bool) ([]*Result, error) {
	return s.verifyMany(ctx, scope, emails, fresh, true)
}

// VerifyManyUnmetered is VerifyMany without the hourly rate limit, for
// background jobs whose size was already bounded at submission.
func (s *Service) VerifyManyUnmetered(ctx context.Context, scope repositories.ResourceScope, emails []string, fresh bool) ([]*Result, error) {
	return s.verifyMany(ctx, scope, emails, fresh, false)
}

func (s *Service) verifyMany(ctx context.Context, scope repositories.ResourceScope, emails []string, fresh, metered bool) ([]*Result, error) {
	results := make([]*Result, len(emails))
	positions := make(map[string][]int) // normalized address -> input indexes
	var distinct []string

	for i, raw := range emails {
		email, ok := normalizeAddress(raw)
		if !ok {
			r := baseResult(strings.ToLower(strings.TrimSpace(raw)))
			r.Status, r.Score, r.Reason = decide(verdictInput{})
			results[i] = r
			continue
		}
		if _, seen := positions[email]; !seen {
			distinct = append(distinct, email)
		}
		positions[email] = append(positions[email], i)
	}
	if len(distinct) == 0 {
		return results, nil
	}

	if metered && s.opts.RateHourly > 0 {
		if err := s.checkRate(ctx, scope.UserID, len(distinct)); err != nil {
			return nil, err
		}
	}

	suppressed, bounced := s.overlay(scope, distinct)

	var toCompute []string
	for _, e := range distinct {
		_, sup := suppressed[e]
		_, bnc := bounced[e]
		if !sup && !bnc {
			toCompute = append(toCompute, e)
		}
	}
	intrinsic := s.computeMany(ctx, toCompute, fresh)

	for _, e := range distinct {
		_, sup := suppressed[e]
		_, bnc := bounced[e]
		var base *Result
		if sup || bnc {
			base = baseResult(e)
			base.Checks.Syntax = true
			base.Status, base.Score = StatusInvalid, 0
			base.Suppressed, base.PreviouslyBounced = sup, bnc
			if sup {
				base.Reason = "address is on the suppression list"
			} else {
				base.Reason = "address previously hard-bounced"
			}
		} else {
			base = intrinsic[e]
		}
		for _, idx := range positions[e] {
			cp := *base
			results[idx] = &cp
		}
	}
	return results, nil
}

// normalizeAddress parses raw as an address and returns its lowercased bare
// form; ok is false when raw is not a syntactically valid address.
func normalizeAddress(raw string) (string, bool) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || !strings.Contains(addr.Address, "@") {
		return "", false
	}
	return strings.ToLower(addr.Address), true
}

// overlay loads the tenant's suppression and hard-bounce history in one query
// each. Lookup errors fail open, as the single-address path always has.
func (s *Service) overlay(scope repositories.ResourceScope, emails []string) (map[string]struct{}, map[string]struct{}) {
	sup, bnc := map[string]struct{}{}, map[string]struct{}{}
	if s.suppressions != nil {
		if m, err := s.suppressions.SuppressedSet(scope, emails); err == nil {
			sup = m
		} else {
			logger.Warn("email verify: suppression lookup failed", "op", "SuppressedSet",
				"workspace_id", workspaceIDOf(scope), "error", err)
		}
	}
	if s.bounces != nil {
		if m, err := s.bounces.HardBouncedSet(scope, emails); err == nil {
			bnc = m
		} else {
			logger.Warn("email verify: hard bounce lookup failed", "op", "HardBouncedSet",
				"workspace_id", workspaceIDOf(scope), "error", err)
		}
	}
	return sup, bnc
}

// workspaceIDOf returns the scope's workspace id for logging (0 when unset).
func workspaceIDOf(scope repositories.ResourceScope) uint {
	if scope.WorkspaceID == nil {
		return 0
	}
	return *scope.WorkspaceID
}

// computeMany resolves intrinsic results, grouping by domain so each domain's
// DNS is looked up once, with at most Concurrency domains in flight.
func (s *Service) computeMany(ctx context.Context, emails []string, fresh bool) map[string]*Result {
	out := make(map[string]*Result, len(emails))
	if len(emails) == 0 {
		return out
	}
	byDomain := make(map[string][]string)
	var domains []string
	for _, e := range emails {
		d := e[strings.LastIndex(e, "@")+1:]
		if _, ok := byDomain[d]; !ok {
			domains = append(domains, d)
		}
		byDomain[d] = append(byDomain[d], e)
	}

	workers := s.opts.Concurrency
	if workers <= 0 {
		workers = 16
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for _, d := range domains {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(domain string, addrs []string) {
			defer wg.Done()
			defer func() { <-sem }()
			local := s.computeDomain(ctx, domain, addrs, fresh)
			mu.Lock()
			for k, v := range local {
				out[k] = v
			}
			mu.Unlock()
		}(d, byDomain[d])
	}
	wg.Wait()

	// Anything skipped by cancellation is reported as undetermined.
	for _, e := range emails {
		if out[e] == nil {
			r := baseResult(e)
			r.Checks.Syntax = true
			r.Status, r.Score, r.Reason = decide(verdictInput{SyntaxOK: true, DNS: dnsTempErr})
			out[e] = r
		}
	}
	return out
}

// computeDomain serves cached addresses and computes the rest with a single
// DNS resolution for the domain.
func (s *Service) computeDomain(ctx context.Context, domain string, addrs []string, fresh bool) map[string]*Result {
	out := make(map[string]*Result, len(addrs))
	var pending []string
	for _, e := range addrs {
		if !fresh {
			if cached := s.getCache(ctx, e); cached != nil {
				cached.Cached = true
				out[e] = cached
				continue
			}
		}
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return out
	}

	disposable := isDisposable(domain)
	var hosts []string
	outcome, nullMX := dnsOK, false
	if !disposable {
		hosts, outcome, nullMX = s.mxHosts(ctx, domain)
	}

	// verdicts is nil when no probe runs at all (SMTP probing disabled, no
	// prober configured, disposable domain, or an inconclusive MX lookup);
	// every pending address then keeps SMTPSkipped.
	verdicts := s.probeVerdicts(ctx, domain, hosts, pending, disposable, outcome)

	for _, e := range pending {
		smtp := SMTPSkipped
		if verdicts != nil {
			if v, ok := verdicts[e]; ok {
				smtp = v
			} else {
				smtp = SMTPUnknown
			}
		}
		out[e] = s.buildResult(e, disposable, outcome, nullMX, smtp)
		s.setCache(ctx, e, out[e])
	}
	return out
}

// probeChunkSize bounds how many addresses go into a single ProbeDomain call.
// The Prober caps one session at 3x its Timeout, so a single call for
// hundreds of same-domain addresses would leave the tail SMTPUnknown.
const probeChunkSize = 20

// probeVerdicts runs the SMTP probe for pending addresses of domain, or
// returns nil when no probe should run at all. It consults and populates the
// per-domain catch-all cache, and chunks pending into groups of at most
// probeChunkSize addresses per Prober.ProbeDomain call: once one chunk
// reports catch-all, the remaining pending addresses are marked
// SMTPAcceptAll without further probing.
func (s *Service) probeVerdicts(ctx context.Context, domain string, hosts, pending []string, disposable bool, outcome dnsOutcome) map[string]SMTPVerdict {
	if !s.opts.SMTPEnabled || s.prober == nil || disposable || outcome != dnsOK {
		return nil
	}

	key := catchAllKey(domain)
	if s.client != nil {
		if v, err := s.client.Get(ctx, key).Result(); err == nil && v == "1" {
			verdicts := make(map[string]SMTPVerdict, len(pending))
			for _, e := range pending {
				verdicts[e] = SMTPAcceptAll
			}
			return verdicts
		}
	}

	verdicts := make(map[string]SMTPVerdict, len(pending))
	for start := 0; start < len(pending); start += probeChunkSize {
		end := start + probeChunkSize
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[start:end]
		res, catchAll := s.prober.ProbeDomain(ctx, domain, hosts, chunk)
		for _, e := range chunk {
			if v, ok := res[e]; ok {
				verdicts[e] = v
			}
		}
		if catchAll {
			if s.client != nil {
				s.client.Set(ctx, key, "1", s.opts.MXTTL)
			}
			for _, e := range pending[end:] {
				verdicts[e] = SMTPAcceptAll
			}
			break
		}
	}
	return verdicts
}

// buildResult assembles the intrinsic result for one syntactically valid address.
func (s *Service) buildResult(email string, disposable bool, outcome dnsOutcome, nullMX bool, smtp SMTPVerdict) *Result {
	at := strings.LastIndex(email, "@")
	local, domain := email[:at], email[at+1:]
	r := baseResult(email)
	r.Checks.Syntax = true
	r.Checks.Disposable = disposable
	r.Checks.RoleAccount = isRoleAccount(local)
	r.Checks.MX = !disposable && outcome == dnsOK
	r.Checks.SMTP = smtp
	r.MailboxVerified = smtp == SMTPDeliverable
	if sug := suggestDomain(domain); sug != "" {
		r.Suggestion = local + "@" + sug
	}
	r.Status, r.Score, r.Reason = decide(verdictInput{
		SyntaxOK: true, Disposable: disposable, Role: r.Checks.RoleAccount,
		DNS: outcome, NullMX: nullMX, SMTP: smtp,
	})
	return r
}

// compute runs the intrinsic checks for one syntactically valid address,
// bypassing the address cache.
func (s *Service) compute(ctx context.Context, email string) *Result {
	return s.computeDomain(ctx, email[strings.LastIndex(email, "@")+1:], []string{email}, true)[email]
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

// checkRate adds n to a per-user hourly counter; fails open on Redis errors.
func (s *Service) checkRate(ctx context.Context, userID uint, n int) error {
	if s.client == nil || n <= 0 {
		return nil
	}
	key := fmt.Sprintf("verify:rl:hour:%d:%s", userID, time.Now().Format("2006010215"))
	total, err := s.client.IncrBy(ctx, key, int64(n)).Result()
	if err != nil {
		return nil
	}
	if total == int64(n) {
		s.client.Expire(ctx, key, time.Hour)
	}
	if total > int64(s.opts.RateHourly) {
		// A rejected batch must not consume quota: give the units back.
		s.client.DecrBy(ctx, key, int64(n))
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

func addrKey(email string) string      { return "verify:addr:" + email }
func mxKey(domain string) string       { return "verify:mx:" + strings.ToLower(domain) }
func catchAllKey(domain string) string { return "verify:catchall:" + strings.ToLower(domain) }
