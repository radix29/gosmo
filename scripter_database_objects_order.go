package gosmo

import (
	"container/heap"
	"fmt"
	"slices"
	"strings"
)

// ============================================================
// Dependency ordering for a whole-database script
// ============================================================

// orderNode is one object in the dependency graph of a database script.
// rank breaks ties between objects that are ready together, so the output is
// deterministic: lower rank first, then schema, then name.
type orderNode struct {
	key          string
	rank         int
	schema, name string
}

// orderEdge says from needs to: to must be created before from. bound marks
// a schema-bound reference, which can never be broken.
type orderEdge struct {
	from, to string
	bound    bool
}

func (a orderNode) less(b orderNode) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if a.schema != b.schema {
		return a.schema < b.schema
	}
	return a.name < b.name
}

type orderHeap []orderNode

func (h orderHeap) Len() int           { return len(h) }
func (h orderHeap) Less(i, j int) bool { return h[i].less(h[j]) }
func (h orderHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *orderHeap) Push(x any)        { *h = append(*h, x.(orderNode)) }
func (h *orderHeap) Pop() any {
	old := *h
	n := old[len(old)-1]
	*h = old[:len(old)-1]
	return n
}

// orderObjects sorts nodes so every object follows what it references
// (Kahn's algorithm, ties by orderNode.less). An edge naming a node that is
// not in the set, and a self-reference, is ignored.
//
// A cycle can only be made of references SQL Server resolves late (a
// procedure calling a procedure, a view of a function that reads the view's
// table), since a cycle of schema-bound ones could not have been created. It
// is broken at the lowest-ordered node whose still-unmet references are all
// unbound, and each broken reference is returned as a warning. A stuck graph
// with no such node is an internal error.
func orderObjects(nodes []orderNode, edges []orderEdge) ([]orderNode, []string, error) {
	byKey := make(map[string]orderNode, len(nodes))
	for _, n := range nodes {
		byKey[n.key] = n
	}
	waiting := make(map[string]map[string]bool, len(nodes)) // node -> unmet dep -> bound
	dependents := map[string][]string{}
	for _, e := range edges {
		if e.from == e.to {
			continue
		}
		if _, ok := byKey[e.from]; !ok {
			continue
		}
		if _, ok := byKey[e.to]; !ok {
			continue
		}
		if waiting[e.from] == nil {
			waiting[e.from] = map[string]bool{}
		}
		if _, dup := waiting[e.from][e.to]; !dup {
			dependents[e.to] = append(dependents[e.to], e.from)
		}
		waiting[e.from][e.to] = waiting[e.from][e.to] || e.bound
	}

	var ready orderHeap
	queued := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if len(waiting[n.key]) == 0 {
			ready = append(ready, n)
			queued[n.key] = true
		}
	}
	heap.Init(&ready)

	out := make([]orderNode, 0, len(nodes))
	var warnings []string
	for len(out) < len(nodes) {
		if ready.Len() == 0 {
			var pick *orderNode
			for _, n := range nodes {
				if queued[n.key] {
					continue
				}
				unbound := true
				for _, bound := range waiting[n.key] {
					if bound {
						unbound = false
						break
					}
				}
				if unbound && (pick == nil || n.less(*pick)) {
					n := n
					pick = &n
				}
			}
			if pick == nil {
				var left []string
				for _, n := range nodes {
					if !queued[n.key] {
						left = append(left, n.key)
					}
				}
				slices.Sort(left)
				return nil, nil, fmt.Errorf("gosmo: script database objects: a cycle of schema-bound references among %s", strings.Join(left, ", "))
			}
			var broken []string
			for dep := range waiting[pick.key] {
				broken = append(broken, byKey[dep].display())
			}
			slices.Sort(broken)
			for _, b := range broken {
				warnings = append(warnings, fmt.Sprintf("dependency cycle broken: %s -> %s", pick.display(), b))
			}
			clear(waiting[pick.key])
			queued[pick.key] = true
			heap.Push(&ready, *pick)
		}
		n := heap.Pop(&ready).(orderNode)
		out = append(out, n)
		for _, d := range dependents[n.key] {
			if _, ok := waiting[d][n.key]; !ok {
				continue
			}
			delete(waiting[d], n.key)
			if len(waiting[d]) == 0 && !queued[d] {
				queued[d] = true
				heap.Push(&ready, byKey[d])
			}
		}
	}
	return out, warnings, nil
}

func (n orderNode) display() string {
	if n.schema == "" {
		return quoteIdent(n.name)
	}
	return qualifiedName(n.schema, n.name)
}
