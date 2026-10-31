package desktop

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

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
