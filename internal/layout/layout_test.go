package layout

import (
	"fmt"
	"strings"
	"testing"
)

func TestGrid(t *testing.T) {
	tests := []struct {
		panes   int
		maxRows int
		want    []int
	}{
		{0, 3, nil},
		{1, 3, []int{1}},
		{2, 3, []int{2}},
		{3, 3, []int{3}},
		{4, 3, []int{2, 2}},
		{5, 3, []int{3, 2}},
		{6, 3, []int{3, 3}},
		{7, 3, []int{3, 2, 2}},
		{8, 3, []int{3, 3, 2}},
		{9, 3, []int{3, 3, 3}},
		{10, 3, []int{3, 3, 2, 2}},
		{12, 3, []int{3, 3, 3, 3}},
		// With maxRows=2.
		{3, 2, []int{2, 1}},
		{4, 2, []int{2, 2}},
		{5, 2, []int{2, 2, 1}},
		{6, 2, []int{2, 2, 2}},
		// With maxRows=1 (all side by side).
		{3, 1, []int{1, 1, 1}},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_panes_max%d", tt.panes, tt.maxRows), func(t *testing.T) {
			got := Grid(tt.panes, tt.maxRows)
			if len(got) != len(tt.want) {
				t.Fatalf("Grid(%d, %d) = %v, want %v", tt.panes, tt.maxRows, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Grid(%d, %d) = %v, want %v", tt.panes, tt.maxRows, got, tt.want)
				}
			}
		})
	}
}

func TestGridPaneCountPreserved(t *testing.T) {
	for panes := 1; panes <= 20; panes++ {
		for maxRows := 1; maxRows <= 5; maxRows++ {
			cols := Grid(panes, maxRows)
			total := 0
			for _, c := range cols {
				total += c
			}
			if total != panes {
				t.Errorf("Grid(%d, %d) = %v, total %d != %d", panes, maxRows, cols, total, panes)
			}
		}
	}
}

func TestGridMaxRowsRespected(t *testing.T) {
	for panes := 1; panes <= 20; panes++ {
		for maxRows := 1; maxRows <= 5; maxRows++ {
			cols := Grid(panes, maxRows)
			for i, c := range cols {
				if c > maxRows {
					t.Errorf("Grid(%d, %d)[%d] = %d, exceeds maxRows", panes, maxRows, i, c)
				}
			}
		}
	}
}

func TestDistribute(t *testing.T) {
	tests := []struct {
		total int
		n     int
		want  []int
	}{
		{200, 1, []int{200}},
		{200, 2, []int{100, 99}},  // 200 - 1 sep = 199, 199/2 = 99 r 1
		{200, 3, []int{66, 66, 66}}, // 200 - 2 sep = 198, 198/3 = 66
		{50, 3, []int{16, 16, 16}},  // 50 - 2 sep = 48, 48/3 = 16
		{51, 3, []int{17, 16, 16}},  // 51 - 2 = 49, 49/3 = 16 r 1
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_%d", tt.total, tt.n), func(t *testing.T) {
			got := distribute(tt.total, tt.n)
			if len(got) != len(tt.want) {
				t.Fatalf("distribute(%d, %d) = %v, want %v", tt.total, tt.n, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("distribute(%d, %d) = %v, want %v", tt.total, tt.n, got, tt.want)
				}
			}
		})
	}
}

func TestDistributeReconstructsTotal(t *testing.T) {
	for total := 10; total <= 300; total += 17 {
		for n := 1; n <= 6; n++ {
			// Skip degenerate cases where there isn't enough space.
			if total < 2*n-1 {
				continue
			}
			parts := distribute(total, n)
			reconstructed := 0
			for _, p := range parts {
				reconstructed += p
			}
			// Add separators back.
			reconstructed += n - 1
			if reconstructed != total {
				t.Errorf("distribute(%d, %d) = %v, reconstructs to %d", total, n, parts, reconstructed)
			}
		}
	}
}

func TestBuildCustomLayoutTrivial(t *testing.T) {
	// Single pane should fall back to "tiled".
	got := BuildCustomLayout(200, 50, []int{1})
	if got != "tiled" {
		t.Errorf("single pane: got %q, want %q", got, "tiled")
	}
}

func TestBuildCustomLayoutTwoPanes(t *testing.T) {
	// 2 panes in 1 column (stacked).
	got := BuildCustomLayout(200, 50, []int{2})
	// Should contain a vertical split [...].
	if !strings.Contains(got, "[") {
		t.Errorf("expected vertical split for 2-pane column, got %q", got)
	}
	// Should start with a 4-char hex checksum.
	if len(got) < 5 || got[4] != ',' {
		t.Errorf("expected checksum prefix, got %q", got[:min(10, len(got))])
	}
}

func TestBuildCustomLayoutMultiColumn(t *testing.T) {
	// 5 panes: [3, 2] columns.
	got := BuildCustomLayout(200, 50, []int{3, 2})
	// Should contain a horizontal split {...}.
	if !strings.Contains(got, "{") {
		t.Errorf("expected horizontal split for multi-column, got %q", got)
	}
	// Should reference pane indices 0 through 4.
	for i := range 5 {
		if !strings.Contains(got, fmt.Sprintf(",%d", i)) {
			// Also check at end of string.
			if !strings.HasSuffix(got, fmt.Sprintf(",%d]}", i)) {
				t.Errorf("expected pane index %d in layout, got %q", i, got)
			}
		}
	}
}

func TestChecksum(t *testing.T) {
	// Verify checksum is deterministic and non-zero for non-empty input.
	s := "200x50,0,0{100x50,0,0,0,99x50,101,0,1}"
	c1 := checksum(s)
	c2 := checksum(s)
	if c1 != c2 {
		t.Error("checksum not deterministic")
	}
	if c1 == 0 {
		t.Error("checksum should not be zero for non-empty input")
	}
}

func TestBuildCustomLayoutChecksumValid(t *testing.T) {
	// Verify the checksum in the output matches the body.
	layout := BuildCustomLayout(200, 50, []int{3, 2})
	parts := strings.SplitN(layout, ",", 2)
	if len(parts) < 2 {
		t.Fatalf("unexpected layout format: %q", layout)
	}
	checksumStr := parts[0]
	body := parts[1]

	var expectedCsum uint16
	fmt.Sscanf(checksumStr, "%x", &expectedCsum)
	actualCsum := checksum(body)
	if expectedCsum != actualCsum {
		t.Errorf("checksum mismatch: header=%04x, computed=%04x for body %q", expectedCsum, actualCsum, body)
	}
}
