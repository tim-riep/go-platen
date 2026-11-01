// Package printer sends data streams to network printers over raw TCP
// (port 9100, also known as AppSocket or JetDirect). Printers are reached
// through the regular local network, not through the mesh.
package printer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Dialer opens connections; *net.Dialer implements it.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Options configures timeouts.
type Options struct {
	// DialTimeout bounds connection setup. Default 10s.
	DialTimeout time.Duration
	// WriteTimeout bounds a single write. Printers stop reading while busy,
	// so this is an idle timeout rather than a total timeout. Default 2m.
	WriteTimeout time.Duration
	// DrainTimeout bounds the wait for the printer to close the connection
	// after all data was sent. Default 10s.
	DrainTimeout time.Duration
}

func (o *Options) defaults() {
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 2 * time.Minute
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = 10 * time.Second
	}
}

// SendError is returned by Send. Written tells how many bytes reached the
// connection before the error; zero means the printer certainly received
// nothing and the job can be retried safely.
type SendError struct {
	Written int64
	Err     error
}

func (e *SendError) Error() string {
	if e.Written == 0 {
		return fmt.Sprintf("printer: %v", e.Err)
	}
	return fmt.Sprintf("printer: %v after %d bytes", e.Err, e.Written)
}

func (e *SendError) Unwrap() error { return e.Err }

// RefusedError means the printer answered but refused the job, so nothing
// was printed. Retryable tells whether trying again later may succeed,
// e.g. when the printer is busy.
type RefusedError struct {
	Retryable bool
	Err       error
}

func (e *RefusedError) Error() string { return fmt.Sprintf("printer refused the job: %v", e.Err) }

func (e *RefusedError) Unwrap() error { return e.Err }

// Send connects to addr and writes r to the printer.
func Send(ctx context.Context, d Dialer, addr string, r io.Reader, o Options) (int64, error) {
	o.defaults()
	dctx, cancel := context.WithTimeout(ctx, o.DialTimeout)
	conn, err := d.DialContext(dctx, "tcp", addr)
	cancel()
	if err != nil {
		return 0, &SendError{Err: err}
	}
	defer conn.Close()

	// Abort blocked writes when ctx is canceled.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	w := &deadlineWriter{conn: conn, timeout: o.WriteTimeout}
	n, err := io.Copy(w, r)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return n, &SendError{Written: n, Err: err}
	}
	// Half-close and give the printer time to consume the data. Some
	// printers drop data that is still buffered when the connection is
	// reset, so wait until the printer closes its side.
	if tc, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = tc.CloseWrite()
		_ = conn.SetReadDeadline(time.Now().Add(o.DrainTimeout))
		_, _ = io.Copy(io.Discard, conn)
	}
	return n, nil
}

type deadlineWriter struct {
	conn    net.Conn
	timeout time.Duration
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
		return 0, err
	}
	return w.conn.Write(p)
}

// Probe checks whether the printer accepts connections.
func Probe(ctx context.Context, d Dialer, addr string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// IsSafeToRetry reports whether a Send error guarantees that no data
// reached the printer.
func IsSafeToRetry(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.Written == 0
}
