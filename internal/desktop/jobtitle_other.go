//go:build !windows && !linux

package desktop

// jobTitle has no print queue to ask outside Windows.
func jobTitle(printerID, printer string) string { return "" }
