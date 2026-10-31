package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
)

// Client of the server's client API (/client/v1). The server knows the
// device by its NetBird peer, so there is no token.
type Client struct {
	BaseURL string
	Name    string // sent as X-Platen-Client
	HTTP    *http.Client
}

// ServerURL accepts "host", "host:port" or "http(s)://host[:port]".
func ServerURL(address string) (string, error) {
	s := strings.TrimSpace(address)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("invalid server address %q", address)
	}
	return u.Scheme + "://" + u.Host, nil
}

// NewClient returns a client for the server at address.
func NewClient(address, name string) (*Client, error) {
	base, err := ServerURL(address)
	if err != nil {
		return nil, err
	}
	return &Client{BaseURL: base, Name: name, HTTP: &http.Client{Timeout: 5 * time.Minute}}, nil
}

// TransportError is a failure to reach the server.
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Retryable reports whether sending the same request later may succeed.
func Retryable(err error) bool {
	var te *TransportError
	if errors.As(err, &te) {
		return true
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		// 403: the server may not know the device yet (NetBird user sync).
		return ae.Status == http.StatusForbidden || ae.Status == http.StatusRequestTimeout ||
			ae.Status == http.StatusTooManyRequests || ae.Status >= 500
	}
	return false
}

func (c *Client) Me(ctx context.Context) (*api.ClientMe, error) {
	var me api.ClientMe
	return &me, c.do(ctx, http.MethodGet, "/client/v1/me", nil, nil, &me)
}

func (c *Client) Printers(ctx context.Context) ([]api.Printer, error) {
	var l api.PrinterList
	if err := c.do(ctx, http.MethodGet, "/client/v1/printers", nil, nil, &l); err != nil {
		return nil, err
	}
	return l.Printers, nil
}

// Submit sends the document. The idempotency key makes retries safe.
func (c *Client) Submit(ctx context.Context, printerID, title string, copies int, path, contentType, key string) (*api.Job, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	q := url.Values{"title": {truncate(title, 255)}}
	if copies > 1 {
		q.Set("copies", strconv.Itoa(min(copies, 999)))
	}
	h := http.Header{"Content-Type": {contentType}, "Idempotency-Key": {key}}
	var job api.Job
	return &job, c.do(ctx, http.MethodPost, "/client/v1/printers/"+url.PathEscape(printerID)+"/jobs?"+q.Encode(), h, f, &job)
}

func (c *Client) do(ctx context.Context, method, path string, h http.Header, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("X-Platen-Client", c.Name)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &TransportError{Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return &TransportError{Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var eb api.ErrorBody
		if json.Unmarshal(data, &eb) == nil && eb.Error != nil {
			eb.Error.Status = resp.StatusCode
			return eb.Error
		}
		return &api.Error{Status: resp.StatusCode, Code: "http", Message: resp.Status}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("unexpected server response: %w", err)
	}
	return nil
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	out := ""
	for _, r := range s {
		if len(out)+len(string(r)) > n {
			break
		}
		out += string(r)
	}
	return out
}
