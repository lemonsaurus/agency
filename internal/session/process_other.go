//go:build !linux && !darwin

package session

func processDescendsFrom(_, _ int) bool {
	return false
}
