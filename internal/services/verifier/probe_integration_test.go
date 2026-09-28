// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/goposta/posta/internal/storage/repositories"
)

type fakeProber struct {
	verdicts   map[string]SMTPVerdict
	catchAll   bool
	calls      int
	chunkSizes []int // len(emails) passed to each ProbeDomain call, in order
}

func (f *fakeProber) ProbeDomain(_ context.Context, _ string, _ []string, emails []string) (map[string]SMTPVerdict, bool) {
	f.calls++
	f.chunkSizes = append(f.chunkSizes, len(emails))
	out := make(map[string]SMTPVerdict, len(emails))
	for _, e := range emails {
		if f.catchAll {
			out[e] = SMTPAcceptAll
		} else {
			out[e] = f.verdicts[e]
		}
	}
	return out, f.catchAll
}

func TestProbeResultsFeedVerdict(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, SMTPEnabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	p := &fakeProber{verdicts: map[string]SMTPVerdict{"a@x.com": SMTPDeliverable, "b@x.com": SMTPUndeliverable, "info@x.com": SMTPDeliverable}}
	s.SetProber(p)
	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1},
		[]string{"a@x.com", "b@x.com", "info@x.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != StatusValid || !res[0].MailboxVerified || res[0].Checks.SMTP != SMTPDeliverable {
		t.Errorf("a: %+v", res[0])
	}
	if res[1].Status != StatusInvalid || res[1].Reason != "mailbox does not exist" {
		t.Errorf("b: %+v", res[1])
	}
	if res[2].Status != StatusRisky {
		t.Errorf("role: %+v", res[2])
	}
	if p.calls != 1 {
		t.Errorf("one probe session per domain expected, got %d", p.calls)
	}
}

func TestCatchAllDomain(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, SMTPEnabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	s.SetProber(&fakeProber{catchAll: true})
	res, _ := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, []string{"z@x.com"}, false)
	if res[0].Status != StatusAcceptAll {
		t.Fatalf("got %+v", res[0])
	}
}

func TestProbeDisabledSkips(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	p := &fakeProber{}
	s.SetProber(p) // present but disabled by options
	res, _ := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, []string{"a@x.com"}, false)
	if p.calls != 0 || res[0].Checks.SMTP != SMTPSkipped {
		t.Fatalf("probe must not run when disabled: calls=%d smtp=%s", p.calls, res[0].Checks.SMTP)
	}
}

// TestProbeChunkingFillsAllResults checks the controller ruling on top of the
// brief: ProbeDomain is called in chunks of at most probeChunkSize (20)
// pending addresses, so a large same-domain batch does not leave the tail of
// one oversized session SMTPUnknown.
func TestProbeChunkingFillsAllResults(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, SMTPEnabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	p := &fakeProber{verdicts: map[string]SMTPVerdict{}}
	emails := make([]string, 45)
	for i := range emails {
		e := fmt.Sprintf("u%02d@x.com", i)
		emails[i] = e
		p.verdicts[e] = SMTPDeliverable
	}
	s.SetProber(p)

	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, emails, false)
	if err != nil {
		t.Fatal(err)
	}

	if p.calls != 3 {
		t.Fatalf("expected 3 ProbeDomain calls for 45 addresses, got %d (sizes=%v)", p.calls, p.chunkSizes)
	}
	for i, n := range p.chunkSizes {
		if n > 20 {
			t.Errorf("chunk %d has %d emails, want <= 20", i, n)
		}
	}
	for i, r := range res {
		if r.Checks.SMTP != SMTPDeliverable {
			t.Errorf("res[%d] (%s) SMTP = %s, want deliverable", i, emails[i], r.Checks.SMTP)
		}
	}
}
