//go:build !windows

package main

// loginItem does nothing outside Windows.
type loginItem struct{}

func (loginItem) Enabled() bool            { return false }
func (loginItem) SetEnabled(on bool) error { return nil }
