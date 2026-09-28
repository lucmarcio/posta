// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	_ "embed"
	"strings"
)

//go:embed data/disposable_domains.txt
var disposableList string

// disposableDomains merges the embedded community blocklist with a few
// hand-picked extras. Built once at package init.
var disposableDomains = func() map[string]bool {
	set := make(map[string]bool, 8192)
	for _, line := range strings.Split(disposableList, "\n") {
		d := strings.ToLower(strings.TrimSpace(line))
		if d == "" || strings.HasPrefix(d, "#") {
			continue
		}
		set[d] = true
	}
	for d := range extraDisposable {
		set[d] = true
	}
	return set
}()

// extraDisposable is a representative set of hand-picked throwaway email
// providers merged into disposableDomains alongside the embedded list.
var extraDisposable = map[string]bool{
	"mailinator.com":    true,
	"guerrillamail.com": true,
	"guerrillamail.net": true,
	"sharklasers.com":   true,
	"grr.la":            true,
	"10minutemail.com":  true,
	"10minutemail.net":  true,
	"tempmail.com":      true,
	"temp-mail.org":     true,
	"throwawaymail.com": true,
	"yopmail.com":       true,
	"yopmail.net":       true,
	"getnada.com":       true,
	"nada.email":        true,
	"trashmail.com":     true,
	"trashmail.de":      true,
	"dispostable.com":   true,
	"maildrop.cc":       true,
	"mailnesia.com":     true,
	"fakeinbox.com":     true,
	"spamgourmet.com":   true,
	"mintemail.com":     true,
	"mohmal.com":        true,
	"emailondeck.com":   true,
	"tempinbox.com":     true,
	"discard.email":     true,
	"mailcatch.com":     true,
	"inboxbear.com":     true,
	"33mail.com":        true,
	"burnermail.io":     true,
}

// roleLocalParts are mailbox names that usually point at a function/team rather
// than a person. Deliverable, but risky for cold/marketing sends.
var roleLocalParts = map[string]bool{
	"admin":         true,
	"administrator": true,
	"abuse":         true,
	"billing":       true,
	"contact":       true,
	"help":          true,
	"hello":         true,
	"hostmaster":    true,
	"info":          true,
	"mail":          true,
	"marketing":     true,
	"no-reply":      true,
	"noreply":       true,
	"office":        true,
	"postmaster":    true,
	"sales":         true,
	"security":      true,
	"support":       true,
	"team":          true,
	"webmaster":     true,
	"nepasrepondre": true,
}

// isDisposable reports whether the domain, or any parent domain above the TLD,
// is a known throwaway provider (x.mailinator.com matches mailinator.com).
func isDisposable(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	for strings.Contains(d, ".") {
		if disposableDomains[d] {
			return true
		}
		d = d[strings.IndexByte(d, '.')+1:]
	}
	return false
}

// isRoleAccount reports whether the local part is a role/function mailbox.
func isRoleAccount(local string) bool {
	return roleLocalParts[strings.ToLower(local)]
}
