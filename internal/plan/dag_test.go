package plan

import (
	"errors"
	"reflect"
	"testing"
)

func TestTopoSort_Linear(t *testing.T) {
	nodes := []DAGNode{
		{Name: "a"},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "c", DependsOn: []string{"b"}},
	}
	got, err := TopoSort(nodes)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestTopoSort_DeterministicTieBreak(t *testing.T) {
	// Three roots, no dependencies: lex order must win regardless of input order.
	nodes := []DAGNode{
		{Name: "charlie"},
		{Name: "alpha"},
		{Name: "bravo"},
	}
	got, err := TopoSort(nodes)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestTopoSort_DiamondDeterministic(t *testing.T) {
	// a -> {b, c} -> d. After "a", both b and c are ready: lex picks "b".
	nodes := []DAGNode{
		{Name: "d", DependsOn: []string{"b", "c"}},
		{Name: "c", DependsOn: []string{"a"}},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "a"},
	}
	got, err := TopoSort(nodes)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	want := []string{"a", "b", "c", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestTopoSort_Cycle(t *testing.T) {
	nodes := []DAGNode{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	}
	_, err := TopoSort(nodes)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want CycleError, got %v", err)
	}
	if ce.From == "" || ce.To == "" {
		t.Errorf("cycle error missing endpoints: %+v", ce)
	}
	if ce.From == ce.To {
		t.Errorf("cycle endpoints should differ for a→b→a: %+v", ce)
	}
}

func TestTopoSort_SelfLoop(t *testing.T) {
	nodes := []DAGNode{{Name: "a", DependsOn: []string{"a"}}}
	_, err := TopoSort(nodes)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want CycleError, got %v", err)
	}
	if ce.From != "a" || ce.To != "a" {
		t.Errorf("self-loop endpoints = %s -> %s, want a -> a", ce.From, ce.To)
	}
}

func TestTopoSort_MissingDep(t *testing.T) {
	nodes := []DAGNode{{Name: "a", DependsOn: []string{"ghost"}}}
	_, err := TopoSort(nodes)
	var me *MissingDepError
	if !errors.As(err, &me) {
		t.Fatalf("want MissingDepError, got %v", err)
	}
	if me.From != "a" || me.Missing != "ghost" {
		t.Errorf("missing dep error = %+v", me)
	}
}

func TestTopoSort_LongCycle(t *testing.T) {
	// a -> b -> c -> a
	nodes := []DAGNode{
		{Name: "a", DependsOn: []string{"c"}},
		{Name: "b", DependsOn: []string{"a"}},
		{Name: "c", DependsOn: []string{"b"}},
	}
	_, err := TopoSort(nodes)
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("want CycleError, got %v", err)
	}
}

func TestTopoSort_Empty(t *testing.T) {
	got, err := TopoSort(nil)
	if err != nil {
		t.Fatalf("topo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want empty, got %v", got)
	}
}
