package gosmo

import (
	"context"
	"errors"
	"testing"
)

// seqFrom is the single implementation behind all 75 *Seq() iterators, so
// these cover the three behaviors every one of them inherits: the happy
// path, a failed fetch, and an early break out of the range loop.

func TestSeqFromYieldsEveryItem(t *testing.T) {
	want := []int{1, 2, 3}
	fetch := func(context.Context) ([]int, error) { return want, nil }

	var got []int
	for v, err := range seqFrom(context.Background(), fetch) {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got = append(got, v)
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A failed fetch yields exactly one pair — the zero value and the error —
// so a caller that only checks err never sees a bogus item.
func TestSeqFromYieldsFetchError(t *testing.T) {
	wantErr := errors.New("boom")
	fetch := func(context.Context) ([]*Database, error) { return nil, wantErr }

	n := 0
	for v, err := range seqFrom(context.Background(), fetch) {
		n++
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if v != nil {
			t.Fatalf("value = %v, want the zero value alongside an error", v)
		}
	}
	if n != 1 {
		t.Fatalf("yielded %d times, want exactly 1", n)
	}
}

// Breaking out of the range loop must stop the yields rather than run them
// to completion. Note what this does NOT buy: the fetch has already run, so
// the query cost and the memory for the whole collection are already paid —
// see TestSeqFromBreakDoesNotAvoidTheFetch and iter.go's package comment.
func TestSeqFromStopsOnBreak(t *testing.T) {
	fetch := func(context.Context) ([]int, error) { return []int{1, 2, 3, 4, 5}, nil }

	n := 0
	for range seqFrom(context.Background(), fetch) {
		n++
		if n == 2 {
			break
		}
	}
	if n != 2 {
		t.Fatalf("yielded %d times after breaking at 2", n)
	}
}

// The fetch is deferred to the range, not run when the iterator is built,
// so the ctx a *Seq method captured is only honored if it's read at range
// time — the contract iter.go's package comment states.
func TestSeqFromDefersFetchUntilRanged(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context) ([]int, error) {
		calls++
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	seq := seqFrom(ctx, fetch)
	if calls != 0 {
		t.Fatalf("fetch ran %d times before the iterator was ranged over", calls)
	}

	cancel()
	for _, err := range seq {
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	}
	if calls != 1 {
		t.Fatalf("fetch ran %d times, want 1", calls)
	}
}

// Breaking early skips the remaining yields but cannot avoid the fetch: it
// already ran, in full, before the first yield. This pins the "deferred, not
// streaming" contract iter.go documents — a caller must not read an early
// break as bounding either the query or the memory.
func TestSeqFromBreakDoesNotAvoidTheFetch(t *testing.T) {
	fetched := 0
	fetch := func(context.Context) ([]int, error) {
		fetched++
		return []int{1, 2, 3, 4, 5}, nil
	}

	for range seqFrom(context.Background(), fetch) {
		break // on the very first item
	}
	if fetched != 1 {
		t.Fatalf("fetch ran %d times, want 1 — the whole collection is fetched before the first yield", fetched)
	}
}

// fetch runs once per range, not once per iterator value, so the same
// iterator ranged twice queries twice and may see different results. Callers
// needing one snapshot for two passes must use the ...Context method.
func TestSeqFromRefetchesOnEachRange(t *testing.T) {
	calls := 0
	fetch := func(context.Context) ([]int, error) {
		calls++
		return []int{calls}, nil
	}

	seq := seqFrom(context.Background(), fetch)

	var first, second []int
	for v := range seq {
		first = append(first, v)
	}
	for v := range seq {
		second = append(second, v)
	}

	if calls != 2 {
		t.Fatalf("fetch ran %d times across two ranges, want 2", calls)
	}
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] {
		t.Fatalf("first = %v, second = %v — each range must run its own fetch", first, second)
	}
}
