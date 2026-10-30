package server

import "sync"

// notifier wakes up long polling lease requests when jobs may have become
// available. Waiters take the current channel before checking for work, so
// a broadcast between the check and the wait is never missed.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier {
	return &notifier{ch: make(chan struct{})}
}

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}
