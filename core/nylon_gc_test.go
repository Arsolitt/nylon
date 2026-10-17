package core

import (
	"testing"
	"time"

	"github.com/encodeous/nylon/state"
)

func TestAdvanceHsStale(t *testing.T) {
	demote := 150 * time.Second
	debounce := 30 * time.Second
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	t.Run("fresh neighbour below demote stays clean", func(t *testing.T) {
		neigh := &state.Neighbour{}
		demoted, recovered := advanceHsStale(neigh, now, time.Minute, demote, debounce)
		if demoted || recovered {
			t.Fatalf("flags = (%v, %v), want (false, false)", demoted, recovered)
		}
		if neigh.HsStale || !neigh.HsStaleSince.IsZero() {
			t.Fatalf("state = (HsStale %v, HsStaleSince %v), want (false, zero)", neigh.HsStale, neigh.HsStaleSince)
		}
	})

	t.Run("demotion arms, debounces, fires once", func(t *testing.T) {
		neigh := &state.Neighbour{}
		demoted, _ := advanceHsStale(neigh, now, demote+time.Second, demote, debounce)
		if demoted {
			t.Fatal("first tick past demote must not demote yet")
		}
		if neigh.HsStaleSince.IsZero() {
			t.Fatal("HsStaleSince must be armed on the first stale tick")
		}
		demoted, _ = advanceHsStale(neigh, now.Add(debounce), demote+debounce+time.Second, demote, debounce)
		if !demoted || !neigh.HsStale {
			t.Fatalf("after debounce: demoted %v, HsStale %v, want true true", demoted, neigh.HsStale)
		}
		demoted, recovered := advanceHsStale(neigh, now.Add(2*debounce), demote+2*debounce, demote, debounce)
		if demoted || recovered {
			t.Fatalf("steady stale state re-fired a transition: (%v, %v)", demoted, recovered)
		}
		if !neigh.HsStale {
			t.Fatal("HsStale must stay set while stale")
		}
	})

	t.Run("recovery clears a demoted neighbour", func(t *testing.T) {
		neigh := &state.Neighbour{HsStale: true, HsStaleSince: now.Add(-time.Hour)}
		demoted, recovered := advanceHsStale(neigh, now, time.Minute, demote, debounce)
		if demoted || !recovered {
			t.Fatalf("flags = (%v, %v), want (false, true)", demoted, recovered)
		}
		if neigh.HsStale || !neigh.HsStaleSince.IsZero() {
			t.Fatalf("state = (HsStale %v, HsStaleSince %v), want (false, zero)", neigh.HsStale, neigh.HsStaleSince)
		}
	})

	t.Run("armed but not demoted is cleared without recovery", func(t *testing.T) {
		neigh := &state.Neighbour{HsStaleSince: now.Add(-time.Second)}
		demoted, recovered := advanceHsStale(neigh, now, time.Minute, demote, debounce)
		if demoted || recovered {
			t.Fatalf("flags = (%v, %v), want (false, false)", demoted, recovered)
		}
		if neigh.HsStale || !neigh.HsStaleSince.IsZero() {
			t.Fatalf("armed state not cleared: HsStale %v, HsStaleSince %v", neigh.HsStale, neigh.HsStaleSince)
		}
	})

	t.Run("disabled stage recovers a stale neighbour", func(t *testing.T) {
		neigh := &state.Neighbour{HsStale: true, HsStaleSince: now.Add(-time.Hour)}
		demoted, recovered := advanceHsStale(neigh, now, time.Hour, 0, debounce)
		if demoted || !recovered {
			t.Fatalf("flags = (%v, %v), want (false, true)", demoted, recovered)
		}
		if neigh.HsStale || !neigh.HsStaleSince.IsZero() {
			t.Fatalf("state = (HsStale %v, HsStaleSince %v), want (false, zero)", neigh.HsStale, neigh.HsStaleSince)
		}
	})
}
