package gosmo

import (
	"slices"
	"strings"
	"testing"
)

func testNodes(names ...string) []orderNode {
	var out []orderNode
	for _, n := range names {
		out = append(out, orderNode{key: objectKey('o', "dbo", n), rank: 0, schema: "dbo", name: n})
	}
	return out
}

func orderNames(t *testing.T, nodes []orderNode, edges []orderEdge) ([]string, []string) {
	t.Helper()
	got, warn, err := orderObjects(nodes, edges)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, n := range got {
		names = append(names, n.name)
	}
	return names, warn
}

func edge(from, to string, bound bool) orderEdge {
	return orderEdge{from: objectKey('o', "dbo", from), to: objectKey('o', "dbo", to), bound: bound}
}

func TestOrderObjectsDependenciesFirst(t *testing.T) {
	// v3 -> v2 -> v1 -> t; names sort the other way round.
	got, warn := orderNames(t, testNodes("a_v3", "b_v2", "c_v1", "d_t", "z_free"),
		[]orderEdge{edge("a_v3", "b_v2", true), edge("b_v2", "c_v1", true), edge("c_v1", "d_t", true)})
	want := []string{"d_t", "c_v1", "b_v2", "a_v3", "z_free"}
	if !slices.Equal(got, want) || len(warn) != 0 {
		t.Errorf("got %v %v, want %v", got, warn, want)
	}
}

func TestOrderObjectsRankThenName(t *testing.T) {
	nodes := []orderNode{
		{key: "o:dbo.b", rank: 4, schema: "dbo", name: "b"},
		{key: "o:dbo.a", rank: 7, schema: "dbo", name: "a"},
		{key: "o:dbo.c", rank: 4, schema: "dbo", name: "c"},
	}
	got, _ := orderNames(t, nodes, nil)
	if !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Errorf("got %v", got)
	}
}

func TestOrderObjectsIgnoresSelfAndUnknownEdges(t *testing.T) {
	got, warn := orderNames(t, testNodes("a", "b"),
		[]orderEdge{edge("a", "a", false), edge("a", "missing", true)})
	if !slices.Equal(got, []string{"a", "b"}) || len(warn) != 0 {
		t.Errorf("got %v %v", got, warn)
	}
}

func TestOrderObjectsBreaksUnboundCycle(t *testing.T) {
	// p1 <-> p2, and p3 needs p2 (bound edges never broken).
	got, warn := orderNames(t, testNodes("p1", "p2", "p3"),
		[]orderEdge{edge("p1", "p2", false), edge("p2", "p1", false), edge("p3", "p2", true)})
	if !slices.Equal(got, []string{"p1", "p2", "p3"}) {
		t.Errorf("got %v", got)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], "[dbo].[p1] -> [dbo].[p2]") {
		t.Errorf("warnings %v", warn)
	}
}

func TestOrderObjectsBreaksAtNodeWithoutBoundWait(t *testing.T) {
	// v1 (lowest) needs p2 bound; p2 needs v1 unbound: only p2 can be broken.
	got, warn := orderNames(t, testNodes("v1", "p2"),
		[]orderEdge{edge("v1", "p2", true), edge("p2", "v1", false)})
	if !slices.Equal(got, []string{"p2", "v1"}) || len(warn) != 1 {
		t.Errorf("got %v %v", got, warn)
	}
}

func TestOrderObjectsBoundCycleIsAnError(t *testing.T) {
	_, _, err := orderObjects(testNodes("a", "b"), []orderEdge{edge("a", "b", true), edge("b", "a", true)})
	if err == nil || !strings.Contains(err.Error(), "schema-bound") {
		t.Errorf("err = %v", err)
	}
}

func TestOrderObjectsDeterministic(t *testing.T) {
	nodes := testNodes("n1", "n2", "n3", "n4", "n5", "n6")
	edges := []orderEdge{edge("n1", "n4", false), edge("n4", "n1", false), edge("n2", "n6", true)}
	first, _ := orderNames(t, nodes, edges)
	for range 20 {
		if got, _ := orderNames(t, nodes, edges); !slices.Equal(got, first) {
			t.Fatalf("order changed: %v vs %v", got, first)
		}
	}
}
