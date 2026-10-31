package desktop

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os/user"
	"time"
)

// cupsSocket is CUPS' local socket.
const cupsSocket = "/run/cups/cups.sock"

// jobTitle asks CUPS for the name of the job it is sending to the
// printer's queue. CUPS shows job names only to their owners and to its
// system group, so the request goes through the local socket with the
// "PeerCred" authorization of CUPS' own clients: cupsd checks it against
// the socket's peer and knows the service user (a member of lpadmin).
func jobTitle(printerID, _ string) string {
	queue := CUPSQueue(printerID)
	u, err := user.Current()
	if err != nil {
		return ""
	}
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", cupsSocket)
		},
	}}
	body := ippGetJobsRequest("ipp://localhost/printers/" + queue)
	// The job may not be processing at the very first moment.
	for try := 0; try < 5; try++ {
		req, _ := http.NewRequest(http.MethodPost, "http://localhost/printers/"+queue, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/ipp")
		req.Header.Set("Authorization", "PeerCred "+u.Username)
		resp, err := c.Do(req)
		if err != nil {
			return ""
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if jobs, err := ippParseJobs(data); err == nil {
			if name := ippActiveJobName(jobs); name != "" {
				return name
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return ""
}
