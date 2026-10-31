//go:build !windows

package desktop

import "context"

// Outside Windows there are no local printers to receive from; App.Receive
// takes documents directly (tests, development).
type noReceiver struct{}

func newReceiver(ctx context.Context, a *App) receiver { return noReceiver{} }

func (noReceiver) update([]LocalPrinter) string { return "" }
