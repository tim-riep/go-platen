//go:build !windows

package desktop

// jobTitle has no print queue to ask outside Windows.
func jobTitle(printer string) string { return "" }
