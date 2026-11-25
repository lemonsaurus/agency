// Package order keeps the sky's window order: one name per window, first to
// last. The box holds the list; devices send moves and arrange their windows
// to the list they get back.
package order

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Move puts Name right after After, or first when After is empty.
type Move struct {
	Name  string `json:"name"`
	After string `json:"after,omitempty"`
}

// Moves returns the fewest moves that give from the relative order of to,
// over the names both hold. Names only one side holds are new or closed
// windows, not moves. The moves go in to's order, each after a name that is
// already in place, so Apply reproduces to's order on any list that holds
// the same names.
func Moves(from, to []string) []Move {
	to = unique(to)
	position := map[string]int{}
	for i, name := range unique(from) {
		position[name] = i
	}
	var common []string
	for _, name := range to {
		if _, ok := position[name]; ok {
			common = append(common, name)
		}
	}
	stays := steady(common, position)
	var moves []Move
	for i, name := range common {
		if stays[i] {
			continue
		}
		move := Move{Name: name}
		if i > 0 {
			move.After = common[i-1]
		}
		moves = append(moves, move)
	}
	return moves
}

// steady marks the longest run of names, in order, that already sit in
// increasing position: those stay put.
func steady(names []string, position map[string]int) []bool {
	length := make([]int, len(names))
	previous := make([]int, len(names))
	best := -1
	for i, name := range names {
		length[i], previous[i] = 1, -1
		for j := range i {
			if position[names[j]] < position[name] && length[j]+1 > length[i] {
				length[i], previous[i] = length[j]+1, j
			}
		}
		if best < 0 || length[i] > length[best] {
			best = i
		}
	}
	stays := make([]bool, len(names))
	for i := best; i >= 0; i = previous[i] {
		stays[i] = true
	}
	return stays
}

// Apply makes moves on names in turn. A move whose window or anchor is gone,
// closed or renamed meanwhile, is skipped.
func Apply(names []string, moves []Move) []string {
	names = slices.Clone(names)
	for _, move := range moves {
		from := slices.Index(names, move.Name)
		if from < 0 || move.Name == move.After {
			continue
		}
		rest := slices.Delete(slices.Clone(names), from, from+1)
		at := 0
		if move.After != "" {
			anchor := slices.Index(rest, move.After)
			if anchor < 0 {
				continue
			}
			at = anchor + 1
		}
		names = slices.Insert(rest, at, move.Name)
	}
	return names
}

// Place puts name at index, counted from 0 and clamped to the list.
func Place(names []string, name string, index int) []string {
	names = slices.DeleteFunc(slices.Clone(names), func(n string) bool { return n == name })
	index = max(0, min(index, len(names)))
	return slices.Insert(names, index, name)
}

// Rename gives from's place to to. When to already has a place, it keeps
// that one and from's goes.
func Rename(names []string, from, to string) []string {
	names = slices.Clone(names)
	i := slices.Index(names, from)
	switch {
	case i < 0 || from == to:
	case slices.Contains(names, to):
		names = slices.Delete(names, i, i+1)
	default:
		names[i] = to
	}
	return names
}

// Reconcile keeps the names still present in their order, drops the closed
// ones so they leave no gap, and appends new ones in present's order.
func Reconcile(names, present []string) []string {
	present = unique(present)
	var kept []string
	for _, name := range unique(names) {
		if slices.Contains(present, name) {
			kept = append(kept, name)
		}
	}
	for _, name := range present {
		if !slices.Contains(kept, name) {
			kept = append(kept, name)
		}
	}
	return kept
}

// Rank sorts items by their name's place in names, keeping the current order
// among items with the same name and after every listed name for the rest.
func Rank[T any](items []T, names []string, name func(T) string) []T {
	place := map[string]int{}
	for i, n := range names {
		place[n] = i
	}
	rank := func(item T) int {
		if i, ok := place[name(item)]; ok {
			return i
		}
		return len(names)
	}
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(a, b T) int { return rank(a) - rank(b) })
	return sorted
}

func unique(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Store is the box's order, saved to a file on every change so it outlives
// daemon restarts.
type Store struct {
	path  string
	mu    sync.Mutex
	names []string
}

type file struct {
	Windows []string `json:"windows"`
}

// Open loads the order saved at path; a missing file is an empty order.
func Open(path string) (*Store, error) {
	names, err := Read(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, names: names}, nil
}

// Read returns the order saved at path, empty when there is none.
func Read(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved file
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	return saved.Windows, nil
}

// Update replaces the order with change's result and saves it when it
// differs, returning the order now held.
func (s *Store) Update(change func([]string) []string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := change(slices.Clone(s.names))
	if slices.Equal(names, s.names) {
		return slices.Clone(s.names), nil
	}
	data, err := json.MarshalIndent(file{Windows: names}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return nil, err
	}
	s.names = names
	return slices.Clone(names), nil
}
