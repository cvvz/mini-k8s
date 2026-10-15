package controller

import "testing"

func TestExpectations(t *testing.T) {
	e := NewExpectations()
	e.Expect("a", 2, []string{"u1"})
	e.ObserveAdd("a")
	e.ObserveDelete("a", "u1")
	if e.Satisfied("a") {
		t.Fatal("one creation is still pending")
	}
	e.ObserveAdd("a")
	if !e.Satisfied("a") {
		t.Fatal("everything was observed")
	}
	// Raise accumulates, so concurrent creations and deletions all count.
	e.Raise("b", 1, nil)
	e.Raise("b", 1, nil)
	e.Raise("b", 0, []string{"u2"})
	e.ObserveAdd("b")
	e.ObserveDelete("b", "u2")
	if e.Satisfied("b") {
		t.Fatal("the second creation is still pending")
	}
	e.ObserveAdd("b")
	if !e.Satisfied("b") {
		t.Fatal("everything was observed")
	}
	e.Forget("b")
	if !e.Satisfied("b") {
		t.Fatal("forgotten")
	}
}
