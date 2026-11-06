package gosmo

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func queueOf(gens ...func() (string, error)) []pendingObject {
	var q []pendingObject
	for i, g := range gens {
		q = append(q, pendingObject{ScriptedObject{Kind: ScriptedProcedure, Schema: "dbo", Name: string(rune('a' + i))}, g})
	}
	return q
}

func sqlGen(s string) func() (string, error) { return func() (string, error) { return s, nil } }

// Progress starts at 0, rises by one per object and ends at done == total,
// and an ErrUnsupported object counts like any other.
func TestScriptQueueProgress(t *testing.T) {
	q := queueOf(sqlGen("A\n"), func() (string, error) { return "", unsupportedf("gosmo: encrypted") }, sqlGen("C\n"))
	var calls [][2]int
	out, err := scriptQueue(context.Background(), q, func(done, total int) { calls = append(calls, [2]int{done, total}) })
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int{{0, 3}, {1, 3}, {2, 3}, {3, 3}}
	if !slices.Equal(calls, want) {
		t.Fatalf("progress calls = %v, want %v", calls, want)
	}
	if len(out) != 3 || out[1].Note == "" || !strings.HasPrefix(out[1].SQL, "-- not scripted: ") || out[2].SQL != "C\n" {
		t.Fatalf("objects = %+v", out)
	}
}

func TestScriptQueueNilProgressAndEmpty(t *testing.T) {
	if out, err := scriptQueue(context.Background(), nil, nil); err != nil || len(out) != 0 {
		t.Fatalf("empty queue = %v, %v", out, err)
	}
	var calls [][2]int
	if _, err := scriptQueue(context.Background(), nil, func(d, n int) { calls = append(calls, [2]int{d, n}) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, [][2]int{{0, 0}}) {
		t.Fatalf("empty queue progress = %v, want [[0 0]]", calls)
	}
	if _, err := scriptQueue(context.Background(), queueOf(sqlGen("A")), nil); err != nil {
		t.Fatal(err)
	}
}

// A cancelled context stops before the next object, and any error other than
// ErrUnsupported ends the script.
func TestScriptQueueStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ran := 0
	q := queueOf(func() (string, error) { ran++; cancel(); return "A", nil }, func() (string, error) { ran++; return "B", nil })
	if _, err := scriptQueue(ctx, q, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran != 1 {
		t.Fatalf("ran %d objects after cancel, want 1", ran)
	}
	boom := errors.New("boom")
	if _, err := scriptQueue(context.Background(), queueOf(func() (string, error) { return "", boom }), nil); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}
