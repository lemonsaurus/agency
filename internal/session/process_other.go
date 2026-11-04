//go:build !linux && !darwin

package session

func processLineage(pid int) map[int]bool {
	return map[int]bool{pid: true}
}
