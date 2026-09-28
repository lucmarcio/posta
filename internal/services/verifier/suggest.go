// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import "strings"

// popularDomains are consumer mailbox providers common in Posta's audience
// (global + Brazil). Order breaks ties.
var popularDomains = []string{
	"gmail.com", "googlemail.com", "hotmail.com", "hotmail.com.br", "outlook.com", "outlook.com.br",
	"live.com", "msn.com", "yahoo.com", "yahoo.com.br", "icloud.com", "me.com", "aol.com",
	"proton.me", "protonmail.com", "uol.com.br", "bol.com.br", "terra.com.br", "ig.com.br",
	"globo.com", "globomail.com", "r7.com", "zipmail.com.br",
}

var popularSet = func() map[string]bool {
	m := make(map[string]bool, len(popularDomains))
	for _, d := range popularDomains {
		m[d] = true
	}
	return m
}()

// suggestDomain proposes a likely intended provider for a mistyped domain
// (Levenshtein distance 1–2 from a popular provider). Advisory only.
func suggestDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if len(d) < 8 || popularSet[d] {
		return ""
	}
	best, bestDist := "", 3
	for _, p := range popularDomains {
		if dist := levenshtein(d, p); dist < bestDist {
			best, bestDist = p, dist
		}
	}
	return best
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
