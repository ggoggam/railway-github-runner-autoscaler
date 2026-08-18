// Package bounded provides a fixed-capacity set for "have I seen this before?"
// checks where unbounded growth would be a leak and a rare false negative is
// harmless.
package bounded

// Set remembers up to a fixed number of recent keys, evicting the oldest first.
//
// It is not safe for concurrent use; callers hold their own lock.
type Set[K comparable] struct {
	size  int
	items map[K]struct{}
	order []K
}

// NewSet returns a Set holding at most size keys. A size below 1 is coerced to 1.
func NewSet[K comparable](size int) *Set[K] {
	if size < 1 {
		size = 1
	}
	return &Set[K]{size: size, items: make(map[K]struct{}, size)}
}

// Add records k and reports whether it was already present.
func (s *Set[K]) Add(k K) bool {
	if _, ok := s.items[k]; ok {
		return true
	}
	if len(s.order) >= s.size {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.items, oldest)
	}
	s.items[k] = struct{}{}
	s.order = append(s.order, k)
	return false
}

// Has reports whether k is currently remembered.
func (s *Set[K]) Has(k K) bool {
	_, ok := s.items[k]
	return ok
}

// Len returns the number of keys currently held.
func (s *Set[K]) Len() int { return len(s.items) }
