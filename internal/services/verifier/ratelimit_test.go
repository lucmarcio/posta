// SPDX-FileCopyrightText: 2026 Jonas Kaninda
// SPDX-License-Identifier: AGPL-3.0-or-later

package verifier

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestCheckRateRejectedBatchDoesNotConsumeQuota(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("skipping: no test redis available: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	s := NewService(client, nil, nil, Options{Enabled: true, RateHourly: 5})
	uid := uint(time.Now().UnixNano()%1_000_000) + 900_000_000
	t.Cleanup(func() {
		keys, _ := client.Keys(ctx, "verify:rl:hour:"+strconv.FormatUint(uint64(uid), 10)+":*").Result()
		if len(keys) > 0 {
			client.Del(ctx, keys...)
		}
	})

	if err := s.checkRate(ctx, uid, 3); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if err := s.checkRate(ctx, uid, 3); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second batch must be rate limited, got %v", err)
	}
	// The rejected batch gave its 3 units back: 2 more still fit in the limit of 5.
	if err := s.checkRate(ctx, uid, 2); err != nil {
		t.Fatalf("a rejected batch must not consume quota: %v", err)
	}
}
