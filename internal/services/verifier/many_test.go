// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/goposta/posta/internal/storage/repositories"
)

type setLookup struct{ set map[string]struct{} }

func (s setLookup) SuppressedSet(_ repositories.ResourceScope, _ []string) (map[string]struct{}, error) {
	return s.set, nil
}
func (s setLookup) HardBouncedSet(_ repositories.ResourceScope, _ []string) (map[string]struct{}, error) {
	return s.set, nil
}

type countingResolver struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *countingResolver) LookupMX(_ context.Context, d string) ([]*net.MX, error) {
	c.mu.Lock()
	c.calls[d]++
	c.mu.Unlock()
	return []*net.MX{{Host: "mx." + d, Pref: 10}}, nil
}
func (c *countingResolver) LookupHost(_ context.Context, d string) ([]string, error) {
	return []string{"192.0.2.1"}, nil
}

func TestVerifyManyOrderDedupAndSyntax(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true, Concurrency: 4})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	in := []string{"b@x.com", "not-an-email", "A@X.com", "b@x.com", "c@y.com"}
	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, in, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(in) {
		t.Fatalf("len = %d", len(res))
	}
	want := []string{"b@x.com", "not-an-email", "a@x.com", "b@x.com", "c@y.com"}
	for i, w := range want {
		if res[i].Email != w {
			t.Errorf("res[%d].Email = %q, want %q", i, res[i].Email, w)
		}
	}
	if res[1].Status != StatusInvalid || res[0].Status != StatusValid {
		t.Fatalf("statuses: %s %s", res[0].Status, res[1].Status)
	}
	if res[0] == res[3] {
		t.Fatal("duplicate inputs must get distinct result copies")
	}
}

func TestVerifyManyOneMXLookupPerDomain(t *testing.T) {
	r := &countingResolver{calls: map[string]int{}}
	s := NewService(nil, nil, nil, Options{Enabled: true, Concurrency: 8})
	s.SetResolver(r)
	var in []string
	for i := 0; i < 60; i++ {
		in = append(in, fmt.Sprintf("u%d@d%d.com", i, i%3))
	}
	if _, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1}, in, false); err != nil {
		t.Fatal(err)
	}
	for d, n := range r.calls {
		if n != 1 {
			t.Errorf("domain %s looked up %d times", d, n)
		}
	}
	if len(r.calls) != 3 {
		t.Fatalf("domains looked up = %d", len(r.calls))
	}
}

func TestVerifyManyOverlay(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	s.suppressions = setLookup{set: map[string]struct{}{"sup@x.com": {}}}
	s.bounces = setLookup{set: map[string]struct{}{"bnc@x.com": {}}}
	res, err := s.VerifyMany(context.Background(), repositories.ResourceScope{UserID: 1},
		[]string{"sup@x.com", "bnc@x.com", "ok@x.com"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Suppressed || res[0].Status != StatusInvalid {
		t.Errorf("suppressed: %+v", res[0])
	}
	if !res[1].PreviouslyBounced || res[1].Status != StatusInvalid {
		t.Errorf("bounced: %+v", res[1])
	}
	if res[2].Status != StatusValid || res[2].Suppressed || res[2].PreviouslyBounced {
		t.Errorf("clean: %+v", res[2])
	}
}

func TestVerifyDelegatesToMany(t *testing.T) {
	s := NewService(nil, nil, nil, Options{Enabled: true})
	s.SetResolver(&countingResolver{calls: map[string]int{}})
	r, err := s.Verify(context.Background(), repositories.ResourceScope{UserID: 1}, " Foo@X.com ", false)
	if err != nil || r.Email != "foo@x.com" || r.Status != StatusValid {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}
