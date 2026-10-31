package desktop

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// tcpReceiver listens on 127.0.0.1 with a port of its own for every
// printer. The Windows spooler connects through the printer's Standard
// TCP/IP port (RAW), sends the PDF and closes the connection.
type tcpReceiver struct {
	ctx context.Context
	app *App

	mu        sync.Mutex
	listeners map[string]*listener // by printer ID
}

type listener struct {
	ln   net.Listener
	port int
	name string // Windows printer name, to look up the job title
}

// listenSpec is a printer to listen for: its Windows name (empty while it
// is not installed) and the port it uses now (0 if none).
type listenSpec struct {
	Name string
	Port int
}

func newTCPReceiver(ctx context.Context, a *App) *tcpReceiver {
	r := &tcpReceiver{ctx: ctx, app: a, listeners: map[string]*listener{}}
	go func() {
		<-ctx.Done()
		r.listen(nil)
	}()
	return r
}

// listen listens for exactly the printers in want and returns their ports.
// A printer keeps its port if the receiver can listen on it; otherwise it
// gets a free one, and the caller moves the printer there. The message
// tells the user about printers it cannot listen for at all.
func (r *tcpReceiver) listen(want map[string]listenSpec) (map[string]int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		want = nil
	}
	for id, l := range r.listeners {
		if _, ok := want[id]; !ok {
			l.ln.Close()
			delete(r.listeners, id)
		}
	}
	ports := map[string]int{}
	var failed []string
	for id, spec := range want {
		if l, ok := r.listeners[id]; ok {
			if spec.Name != "" {
				l.name = spec.Name
			}
			ports[id] = l.port
			continue
		}
		var ln net.Listener
		var err error
		if spec.Port > 0 {
			ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", spec.Port))
			if err != nil {
				r.app.Log.Warn("receiver: port taken, the printer moves to another one", "printer", id, "port", spec.Port, "err", err)
			}
		}
		if ln == nil {
			ln, err = net.Listen("tcp", "127.0.0.1:0")
		}
		if err != nil {
			r.app.Log.Error("receiver: listen", "printer", id, "err", err)
			failed = append(failed, id)
			continue
		}
		l := &listener{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, name: spec.Name}
		r.listeners[id] = l
		ports[id] = l.port
		go r.serve(id, l)
	}
	if len(failed) == 0 {
		return ports, ""
	}
	return ports, "Platen cannot receive jobs for " + strings.Join(failed, ", ") + "."
}

func (r *tcpReceiver) serve(id string, l *listener) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // closed by listen
		}
		r.mu.Lock()
		name := l.name
		r.mu.Unlock()
		go func() {
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(30 * time.Minute))
			// Read at once and ask for the title alongside: while the
			// spooler sends, it holds the printer, so a lookup before
			// reading could wait for the reader, which waits for it.
			titles := make(chan string, 1)
			go func() { titles <- jobTitle(name) }()
			title := func() string {
				select {
				case t := <-titles:
					return t
				case <-time.After(5 * time.Second):
					return ""
				}
			}
			if err := r.app.Receive(id, title, conn); err != nil {
				r.app.Log.Error("receiver: job lost", "printer", id, "err", err)
			}
		}()
	}
}
