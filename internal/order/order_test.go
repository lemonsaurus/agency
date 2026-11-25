package order

import (
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestMovesReproduceAnyOrderWithTheFewestMoves(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		n := r.IntN(9)
		from := make([]string, n)
		for i := range from {
			from[i] = strconv.Itoa(i)
		}
		to := slices.Clone(from)
		r.Shuffle(n, func(i, j int) { to[i], to[j] = to[j], to[i] })
		moves := Moves(from, to)
		if got := Apply(from, moves); !slices.Equal(got, to) {
			t.Fatalf("Apply(%v, Moves(%v, %v) = %v) = %v", from, from, to, moves, got)
		}
		if lis := n - len(moves); lis != longestIncreasing(from, to) {
			t.Fatalf("%v → %v took %d moves", from, to, len(moves))
		}
	}
}

func longestIncreasing(from, to []string) int {
	best := make([]int, len(to))
	longest := 0
	for i := range to {
		best[i] = 1
		for j := range i {
			if slices.Index(from, to[j]) < slices.Index(from, to[i]) {
				best[i] = max(best[i], best[j]+1)
			}
		}
		longest = max(longest, best[i])
	}
	return longest
}

func TestOneMovedWindowIsOneMove(t *testing.T) {
	from := []string{"journalia", "agency", "control", "runtimes"}
	moves := Moves(from, []string{"control", "journalia", "agency", "runtimes"})
	if !slices.Equal(moves, []Move{{Name: "control"}}) {
		t.Fatalf("moves = %v", moves)
	}
	moves = Moves(from, []string{"agency", "journalia", "control", "runtimes"})
	if len(moves) != 1 {
		t.Fatalf("a swap of neighbours took %v", moves)
	}
}

func TestOpenedAndClosedWindowsAreNotMoves(t *testing.T) {
	base := []string{"control", "journalia", "agency"}
	local := []string{"control", "eksamen", "journalia"}
	if moves := Moves(base, local); len(moves) != 0 {
		t.Fatalf("moves = %v", moves)
	}
}

func TestConcurrentMovesFromTwoDevicesBothLand(t *testing.T) {
	box := []string{"journalia", "agency", "control", "runtimes", "secretary"}
	desktop := Moves(box, []string{"control", "journalia", "agency", "runtimes", "secretary"})
	laptop := Moves(box, []string{"journalia", "agency", "control", "secretary", "runtimes"})
	box = Apply(Apply(box, desktop), laptop)
	if want := []string{"control", "journalia", "agency", "secretary", "runtimes"}; !slices.Equal(box, want) {
		t.Fatalf("box = %v, want %v", box, want)
	}
}

func TestMovesAgainstAClosedOrRenamedAnchorAreSkipped(t *testing.T) {
	moves := []Move{{Name: "agency", After: "gone"}, {Name: "gone"}, {Name: "control"}}
	got := Apply([]string{"journalia", "agency", "control"}, moves)
	if want := []string{"control", "journalia", "agency"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReconcileAppendsNewWindowsAndLeavesNoGap(t *testing.T) {
	got := Reconcile([]string{"control", "closed", "journalia"}, []string{"journalia", "fresh", "control"})
	if want := []string{"control", "journalia", "fresh"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestRenameKeepsThePlace(t *testing.T) {
	names := []string{"control", "main", "agency"}
	if got := Rename(names, "main", "journalia"); !slices.Equal(got, []string{"control", "journalia", "agency"}) {
		t.Fatalf("rename = %v", got)
	}
	if got := Rename(names, "main", "agency"); !slices.Equal(got, []string{"control", "agency"}) {
		t.Fatalf("rename onto an existing window = %v", got)
	}
}

func TestPlaceClampsTheIndex(t *testing.T) {
	names := []string{"a", "b", "c"}
	for index, want := range map[int][]string{-3: {"c", "a", "b"}, 1: {"a", "c", "b"}, 9: {"a", "b", "c"}} {
		if got := Place(names, "c", index); !slices.Equal(got, want) {
			t.Errorf("Place(c, %d) = %v, want %v", index, got, want)
		}
	}
}

func TestRankKeepsWindowsOfOneGroupTogetherInTheirOrder(t *testing.T) {
	type window struct{ id, group string }
	windows := []window{{"@1", "journalia"}, {"@2", "control"}, {"@3", "new"}, {"@4", "journalia"}, {"@5", "control"}}
	sorted := Rank(windows, []string{"control", "journalia"}, func(w window) string { return w.group })
	var ids []string
	for _, w := range sorted {
		ids = append(ids, w.id)
	}
	if want := []string{"@2", "@5", "@1", "@4", "@3"}; !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestStoreOutlivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "window-order.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(func([]string) []string { return []string{"control", "journalia"} }); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.Update(func(names []string) []string { return names })
	if err != nil || !slices.Equal(got, []string{"control", "journalia"}) {
		t.Fatalf("order after restart = %v, %v", got, err)
	}
}
