package bounded

import "testing"

func TestAddReportsDuplicates(t *testing.T) {
	s := NewSet[string](4)

	if s.Add("a") {
		t.Fatal("first Add should report not-present")
	}
	if !s.Add("a") {
		t.Fatal("second Add should report already-present")
	}
	if !s.Has("a") {
		t.Fatal("Has should find a")
	}
}

func TestEvictsOldest(t *testing.T) {
	s := NewSet[int](3)
	for i := range 5 {
		s.Add(i)
	}

	if s.Len() != 3 {
		t.Fatalf("want size capped at 3, got %d", s.Len())
	}
	for _, evicted := range []int{0, 1} {
		if s.Has(evicted) {
			t.Errorf("%d should have been evicted", evicted)
		}
	}
	for _, kept := range []int{2, 3, 4} {
		if !s.Has(kept) {
			t.Errorf("%d should still be present", kept)
		}
	}
}

func TestMinimumSize(t *testing.T) {
	s := NewSet[int](0)
	s.Add(1)
	if !s.Has(1) {
		t.Fatal("size <1 should be coerced to 1, not zero capacity")
	}
}
