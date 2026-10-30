package printer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T, handle func(net.Conn)) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

func TestSend(t *testing.T) {
	got := make(chan []byte, 1)
	addr := listen(t, func(c net.Conn) {
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- b
	})
	data := bytes.Repeat([]byte("0123456789"), 100000)
	n, err := Send(context.Background(), &net.Dialer{}, addr, bytes.NewReader(data), Options{})
	if err != nil || n != int64(len(data)) {
		t.Fatalf("send: %d %v", n, err)
	}
	if b := <-got; !bytes.Equal(b, data) {
		t.Fatalf("printer got %d bytes", len(b))
	}
}

func TestSendRefused(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	_, err := Send(context.Background(), &net.Dialer{}, addr, strings.NewReader("x"), Options{DialTimeout: time.Second})
	if err == nil || !IsSafeToRetry(err) {
		t.Fatalf("expected safe-to-retry error, got %v", err)
	}
	if err := Probe(context.Background(), &net.Dialer{}, addr, time.Second); err == nil {
		t.Fatal("probe of closed port succeeded")
	}
}

func TestSendWriteTimeout(t *testing.T) {
	// A printer that never reads.
	addr := listen(t, func(c net.Conn) {
		time.Sleep(5 * time.Second)
		c.Close()
	})
	data := bytes.Repeat([]byte("x"), 64<<20)
	n, err := Send(context.Background(), &net.Dialer{}, addr, bytes.NewReader(data), Options{WriteTimeout: 200 * time.Millisecond})
	var se *SendError
	if !errors.As(err, &se) || n == 0 || IsSafeToRetry(err) {
		t.Fatalf("expected partial write error, got n=%d err=%v", n, err)
	}
}

func TestProbe(t *testing.T) {
	addr := listen(t, func(c net.Conn) { c.Close() })
	if err := Probe(context.Background(), &net.Dialer{}, addr, time.Second); err != nil {
		t.Fatal(err)
	}
}
