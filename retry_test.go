package gosmo

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", errors.New("boom"), false},
		{"bad conn", driver.ErrBadConn, true},
		{"wrapped bad conn", fmt.Errorf("query: %w", driver.ErrBadConn), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryable(c.err); got != c.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestWithRetrySucceedsAfterTransient(t *testing.T) {
	calls := 0
	v, err := withRetry(context.Background(), func() (int, error) {
		calls++
		if calls < 2 {
			return 0, driver.ErrBadConn
		}
		return 99, nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}
	if v != 99 {
		t.Errorf("value = %d, want 99", v)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestWithRetryExhausts(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), func() (int, error) {
		calls++
		return 0, driver.ErrBadConn
	})
	if err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if calls != readRetryAttempts {
		t.Errorf("calls = %d, want %d", calls, readRetryAttempts)
	}
}

func TestWithRetryStopsOnNonRetryable(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), func() (int, error) {
		calls++
		return 0, errors.New("fatal")
	})
	if err == nil {
		t.Fatal("want the non-retryable error back")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-retryable error)", calls)
	}
}

func TestWithRetryHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the backoff sleep
	calls := 0
	_, err := withRetry(ctx, func() (int, error) {
		calls++
		return 0, driver.ErrBadConn
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancelled before retry)", calls)
	}
}
