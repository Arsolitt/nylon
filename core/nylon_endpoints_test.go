package core

import (
	"testing"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

func TestProbeBufTTL(t *testing.T) {
	t.Run("fire-and-forget keeps the default reap", func(t *testing.T) {
		if got := probeBufTTL(0); got != ttlcache.DefaultTTL {
			t.Fatalf("probeBufTTL(0) = %v, want %v", got, ttlcache.DefaultTTL)
		}
	})
	t.Run("buffer outlives the probe timer", func(t *testing.T) {
		for _, tc := range []struct {
			timeout time.Duration
			want    time.Duration
		}{
			{3 * time.Second, 4 * time.Second},
			{10 * time.Second, 11 * time.Second},
		} {
			if got := probeBufTTL(tc.timeout); got != tc.want {
				t.Fatalf("probeBufTTL(%v) = %v, want %v", tc.timeout, got, tc.want)
			}
		}
	})
}
