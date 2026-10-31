package desktop

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeReceiver listens on the named pipe of every managed printer. The
// spooler opens the pipe for each job, writes the PDF and closes it.
type pipeReceiver struct {
	ctx context.Context
	app *App

	mu    sync.Mutex
	pipes map[string]*pipe // by printer ID
}

type pipe struct {
	ln   net.Listener
	name string // Windows printer name, to look up the job title
}

func newReceiver(ctx context.Context, a *App) receiver {
	r := &pipeReceiver{ctx: ctx, app: a, pipes: map[string]*pipe{}}
	go func() {
		<-ctx.Done()
		r.update(nil)
	}()
	return r
}

// pipeSecurity allows the spooler (SYSTEM) and this user to write, and
// refuses access over the network.
func pipeSecurity() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("D:P(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;%s)", u.User.Sid.String()), nil
}

func (r *pipeReceiver) update(printers []LocalPrinter) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		printers = nil
	}
	wanted := map[string]LocalPrinter{}
	for _, p := range printers {
		if id, ok := p.PrinterID(); ok {
			wanted[id] = p
		}
	}
	for id, p := range r.pipes {
		if _, ok := wanted[id]; !ok {
			p.ln.Close()
			delete(r.pipes, id)
		}
	}
	var failed []string
	for id, lp := range wanted {
		if p, ok := r.pipes[id]; ok {
			p.name = lp.Name
			continue
		}
		sd, err := pipeSecurity()
		if err == nil {
			var ln net.Listener
			ln, err = winio.ListenPipe(PortPrefix+id, &winio.PipeConfig{SecurityDescriptor: sd, InputBufferSize: 64 << 10})
			if err == nil {
				p := &pipe{ln: ln, name: lp.Name}
				r.pipes[id] = p
				go r.serve(id, p)
				continue
			}
		}
		r.app.Log.Error("receiver: listen", "pipe", PortPrefix+id, "err", err)
		failed = append(failed, lp.Name)
	}
	if len(failed) == 0 {
		return ""
	}
	// Usually Platen runs a second time, e.g. for another signed-in user.
	return "Platen cannot receive jobs for " + strings.Join(failed, ", ") +
		" because another program, probably Platen of another user, receives them."
}

func (r *pipeReceiver) serve(id string, p *pipe) {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return // closed by update
		}
		r.mu.Lock()
		name := p.name
		r.mu.Unlock()
		go func() {
			defer conn.Close()
			// The job is being printed while the spooler writes, so it is
			// in the printer's queue now.
			title := jobTitle(name)
			if err := r.app.Receive(id, title, conn); err != nil {
				r.app.Log.Error("receiver: job lost", "printer", id, "title", title, "err", err)
			}
		}()
	}
}

var (
	winspool       = windows.NewLazySystemDLL("winspool.drv")
	procOpenPrint  = winspool.NewProc("OpenPrinterW")
	procClosePrint = winspool.NewProc("ClosePrinter")
	procEnumJobs   = winspool.NewProc("EnumJobsW")
)

// jobInfo1 is JOB_INFO_1W.
type jobInfo1 struct {
	JobID        uint32
	PrinterName  *uint16
	MachineName  *uint16
	UserName     *uint16
	Document     *uint16
	Datatype     *uint16
	StatusText   *uint16
	Status       uint32
	Priority     uint32
	Position     uint32
	TotalPages   uint32
	PagesPrinted uint32
	Submitted    windows.Systemtime
}

const jobStatusPrinting = 0x10

// jobTitle returns the document name of the job the printer is printing.
func jobTitle(printer string) string {
	name, err := windows.UTF16PtrFromString(printer)
	if err != nil {
		return ""
	}
	var h windows.Handle
	if r, _, _ := procOpenPrint.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&h)), 0); r == 0 {
		return ""
	}
	defer procClosePrint.Call(uintptr(h))
	// The job may not be marked as printing at the very first moment.
	for try := 0; try < 5; try++ {
		var needed, count uint32
		procEnumJobs.Call(uintptr(h), 0, 64, 1, 0, 0, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
		if needed > 0 {
			buf := make([]byte, needed)
			if r, _, _ := procEnumJobs.Call(uintptr(h), 0, 64, 1, uintptr(unsafe.Pointer(&buf[0])), uintptr(needed),
				uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count))); r != 0 && count > 0 {
				jobs := unsafe.Slice((*jobInfo1)(unsafe.Pointer(&buf[0])), count)
				for _, j := range jobs {
					if j.Status&jobStatusPrinting != 0 && j.Document != nil {
						return windows.UTF16PtrToString(j.Document)
					}
				}
				if count == 1 && jobs[0].Document != nil {
					return windows.UTF16PtrToString(jobs[0].Document)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}
