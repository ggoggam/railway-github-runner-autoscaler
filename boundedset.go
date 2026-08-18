package main

// boundedSet remembers up to size recent keys, evicting the oldest first.
//
// It is used for "have I already seen this?" checks where unbounded growth
// would be a leak and a rare false negative is harmless.
type boundedSet[K comparable] struct {
	size  int
	items map[K]struct{}
	order []K
}

func newBoundedSet[K comparable](size int) *boundedSet[K] {
	if size < 1 {
		size = 1
	}
	return &boundedSet[K]{size: size, items: make(map[K]struct{}, size)}
}

// Add records k and reports whether it was already present.
func (s *boundedSet[K]) Add(k K) bool {
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

func (s *boundedSet[K]) Has(k K) bool {
	_, ok := s.items[k]
	return ok
}

func (s *boundedSet[K]) Len() int { return len(s.items) }
