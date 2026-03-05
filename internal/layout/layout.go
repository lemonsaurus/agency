package layout

import (
	"fmt"
	"strings"
)

// Grid computes the optimal column/row distribution for an ultrawide layout.
// maxRows caps how many panes can stack vertically in one column (vertical
// space is precious on ultrawide monitors). Panes fill columns left-to-right,
// expanding horizontally as needed.
//
// Returns the number of panes in each column, left to right.
// For example, 7 panes with maxRows=3 returns [3, 2, 2].
func Grid(paneCount, maxRows int) []int {
	if paneCount <= 0 {
		return nil
	}
	if maxRows <= 0 {
		maxRows = 3
	}
	if paneCount <= maxRows {
		// Everything fits in one column.
		return []int{paneCount}
	}

	cols := (paneCount + maxRows - 1) / maxRows // ceil(paneCount / maxRows)
	base := paneCount / cols
	extra := paneCount % cols

	columns := make([]int, cols)
	for i := range columns {
		columns[i] = base
		if i < extra {
			columns[i]++
		}
	}
	return columns
}

// BuildCustomLayout generates a tmux custom layout string for the given
// window dimensions and pane distribution. columns is the output of Grid,
// specifying how many panes are in each column.
//
// Returns a complete layout string including the checksum prefix, ready
// to pass to `tmux select-layout`.
func BuildCustomLayout(windowW, windowH int, columns []int) string {
	totalPanes := 0
	for _, c := range columns {
		totalPanes += c
	}
	if totalPanes <= 1 {
		return "tiled" // fallback for trivial cases
	}

	body := buildBody(windowW, windowH, columns)
	csum := checksum(body)
	return fmt.Sprintf("%04x,%s", csum, body)
}

func buildBody(W, H int, columns []int) string {
	nCols := len(columns)

	if nCols == 1 {
		// Single column: vertical split at root level.
		return buildVerticalSplit(W, H, 0, 0, 0, columns[0])
	}

	// Multiple columns: root is a horizontal split {col1,col2,...}.
	colWidths := distribute(W, nCols)
	var parts []string
	paneIdx := 0
	x := 0

	for i, nRows := range columns {
		cw := colWidths[i]
		part := buildColumn(cw, H, x, 0, paneIdx, nRows)
		parts = append(parts, part)
		paneIdx += nRows
		x += cw + 1 // +1 for the separator
	}

	return fmt.Sprintf("%dx%d,0,0{%s}", W, H, strings.Join(parts, ","))
}

func buildColumn(W, H, x, y, startIdx, nRows int) string {
	if nRows == 1 {
		// Single pane in this column — leaf node.
		return fmt.Sprintf("%dx%d,%d,%d,%d", W, H, x, y, startIdx)
	}
	return buildVerticalSplit(W, H, x, y, startIdx, nRows)
}

func buildVerticalSplit(W, H, x, y, startIdx, nRows int) string {
	rowHeights := distribute(H, nRows)
	var parts []string
	cy := y

	for i := range nRows {
		rh := rowHeights[i]
		parts = append(parts, fmt.Sprintf("%dx%d,%d,%d,%d", W, rh, x, cy, startIdx+i))
		cy += rh + 1 // +1 for separator
	}

	return fmt.Sprintf("%dx%d,%d,%d[%s]", W, H, x, y, strings.Join(parts, ","))
}

// distribute divides total into n parts, accounting for n-1 separators
// (each 1 cell wide). Returns the size of each part.
func distribute(total, n int) []int {
	if n <= 0 {
		return nil
	}
	if n == 1 {
		return []int{total}
	}

	available := total - (n - 1) // subtract separators
	if available < n {
		// Degenerate case: not enough space. Give 1 to each.
		result := make([]int, n)
		for i := range result {
			result[i] = 1
		}
		return result
	}

	base := available / n
	extra := available % n

	result := make([]int, n)
	for i := range result {
		result[i] = base
		if i < extra {
			result[i]++
		}
	}
	return result
}

// checksum computes the tmux layout checksum (from layout-custom.c).
func checksum(layout string) uint16 {
	var csum uint16
	for _, c := range []byte(layout) {
		csum = (csum >> 1) | ((csum & 1) << 15)
		csum += uint16(c)
	}
	return csum
}
