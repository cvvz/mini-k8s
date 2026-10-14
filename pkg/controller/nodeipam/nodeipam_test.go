package nodeipam

import "testing"

func TestAllocatorHandsOutDistinctSubnets(t *testing.T) {
	a, err := NewAllocator("10.244.0.0/16", 24)
	if err != nil {
		t.Fatal(err)
	}
	a.Occupy("10.244.2.0/24")
	first, _ := a.Next()
	second, _ := a.Next()
	if first != "10.244.1.0/24" || second != "10.244.3.0/24" {
		t.Fatalf("got %s, %s", first, second)
	}
	a.Release(first)
	if again, _ := a.Next(); again != first {
		t.Fatalf("released subnet not reused: %s", again)
	}
}

func TestAllocatorExhaustion(t *testing.T) {
	a, err := NewAllocator("10.0.0.0/30", 30)
	if err == nil {
		t.Fatalf("a /30 cannot be split into /30s: %v", a)
	}
	a, err = NewAllocator("10.0.0.0/28", 30)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := a.Next(); err != nil {
			t.Fatalf("allocation %d: %v", i, err)
		}
	}
	if _, err := a.Next(); err == nil {
		t.Fatal("exhausted space must fail")
	}
}
