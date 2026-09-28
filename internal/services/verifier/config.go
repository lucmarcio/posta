// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"time"

	"github.com/goposta/posta/internal/config"
	"github.com/goposta/posta/internal/storage/repositories"
	"github.com/redis/go-redis/v9"
)

// FromConfig builds the verifier the same way for the API server and the worker.
func FromConfig(cfg *config.Config, rc *redis.Client, sup *repositories.SuppressionRepository, bnc *repositories.BounceRepository) *Service {
	return NewService(rc, sup, bnc, Options{
		Enabled:     cfg.EmailVerifyEnabled,
		AddrTTL:     time.Duration(cfg.EmailVerifyCacheTTLHours) * time.Hour,
		MXTTL:       time.Duration(cfg.EmailVerifyMXCacheTTLHours) * time.Hour,
		RateHourly:  cfg.EmailVerifyRateHourly,
		Concurrency: cfg.EmailVerifyConcurrency,
	})
}
