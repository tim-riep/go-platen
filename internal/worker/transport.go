package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
)

// Transport is how the worker talks to the server. HTTPTransport is used by
// remote workers, InProcess by the worker built into the server. Errors the
// server reported are returned as *api.Error.
type Transport interface {
	Config(ctx context.Context) (*api.WorkerConfig, error)
	// Lease waits up to wait for a job; it returns nil if there was none.
	Lease(ctx context.Context, printers []string, wait time.Duration) (*api.Lease, error)
	Document(ctx context.Context, jobID string) (io.ReadCloser, error)
	Progress(ctx context.Context, jobID string) (*api.ProgressResponse, error)
	Complete(ctx context.Context, jobID string) error
	Fail(ctx context.Context, jobID string, req api.FailRequest) error
	Heartbeat(ctx context.Context, req api.HeartbeatRequest) error
}

// ---- in-process ----

// Service is the server side worker service (server.WorkerService).
type Service interface {
	Config(ctx context.Context, workerID string) (*api.WorkerConfig, error)
	Lease(ctx context.Context, workerID string, printers []string, wait time.Duration) (*api.Lease, error)
	OpenDocument(ctx context.Context, workerID, jobID string) (io.ReadCloser, error)
	Progress(ctx context.Context, workerID, jobID string) (*api.ProgressResponse, error)
	Complete(ctx context.Context, workerID, jobID string) error
	Fail(ctx context.Context, workerID, jobID string, req api.FailRequest) error
	Heartbeat(ctx context.Context, workerID string, req api.HeartbeatRequest) error
}

// InProcess is the transport of the built-in worker. It calls the server's
// worker service directly, using the same code path as the HTTP worker API
// minus authentication.
type InProcess struct {
	Service  Service
	WorkerID string
}

var _ Transport = (*InProcess)(nil)

func (t *InProcess) Config(ctx context.Context) (*api.WorkerConfig, error) {
	return t.Service.Config(ctx, t.WorkerID)
}

func (t *InProcess) Lease(ctx context.Context, printers []string, wait time.Duration) (*api.Lease, error) {
	return t.Service.Lease(ctx, t.WorkerID, printers, wait)
}

func (t *InProcess) Document(ctx context.Context, jobID string) (io.ReadCloser, error) {
	return t.Service.OpenDocument(ctx, t.WorkerID, jobID)
}

func (t *InProcess) Progress(ctx context.Context, jobID string) (*api.ProgressResponse, error) {
	return t.Service.Progress(ctx, t.WorkerID, jobID)
}

func (t *InProcess) Complete(ctx context.Context, jobID string) error {
	return t.Service.Complete(ctx, t.WorkerID, jobID)
}

func (t *InProcess) Fail(ctx context.Context, jobID string, req api.FailRequest) error {
	return t.Service.Fail(ctx, t.WorkerID, jobID, req)
}

func (t *InProcess) Heartbeat(ctx context.Context, req api.HeartbeatRequest) error {
	return t.Service.Heartbeat(ctx, t.WorkerID, req)
}

// ---- HTTP ----

// HTTPTransport talks to the worker API of a remote server.
type HTTPTransport struct {
	// BaseURL is e.g. "http://platen-server.netbird.cloud:8080".
	BaseURL    string
	Client     *http.Client
	Credential string
	// Timeout bounds requests other than lease and document downloads.
	Timeout time.Duration
}

var _ Transport = (*HTTPTransport)(nil)

func (t *HTTPTransport) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return 30 * time.Second
}

// do sends a request with a JSON body (if in != nil) and decodes a JSON
// response into out (if out != nil). It returns the status code.
func doJSON(ctx context.Context, client *http.Client, method, url, token string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return resp.StatusCode, readError(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// readError converts an error response into an *api.Error.
func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var eb api.ErrorBody
	if json.Unmarshal(b, &eb) == nil && eb.Error != nil {
		eb.Error.Status = resp.StatusCode
		return eb.Error
	}
	return &api.Error{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(b))}
}

func (t *HTTPTransport) call(ctx context.Context, method, path string, in, out any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	return doJSON(ctx, t.Client, method, t.BaseURL+path, t.Credential, in, out)
}

func (t *HTTPTransport) Config(ctx context.Context) (*api.WorkerConfig, error) {
	var cfg api.WorkerConfig
	if _, err := t.call(ctx, http.MethodGet, "/worker/v1/config", nil, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (t *HTTPTransport) Lease(ctx context.Context, printers []string, wait time.Duration) (*api.Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, wait+t.timeout())
	defer cancel()
	var l api.Lease
	status, err := doJSON(ctx, t.Client, http.MethodPost, t.BaseURL+"/worker/v1/lease", t.Credential,
		api.LeaseRequest{Printers: printers, WaitSeconds: int(wait / time.Second)}, &l)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	return &l, nil
}

func (t *HTTPTransport) Document(ctx context.Context, jobID string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.BaseURL+"/worker/v1/jobs/"+jobID+"/document", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.Credential)
	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp.Body, nil
}

func (t *HTTPTransport) Progress(ctx context.Context, jobID string) (*api.ProgressResponse, error) {
	var p api.ProgressResponse
	if _, err := t.call(ctx, http.MethodPost, "/worker/v1/jobs/"+jobID+"/progress", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (t *HTTPTransport) Complete(ctx context.Context, jobID string) error {
	_, err := t.call(ctx, http.MethodPost, "/worker/v1/jobs/"+jobID+"/complete", nil, nil)
	return err
}

func (t *HTTPTransport) Fail(ctx context.Context, jobID string, req api.FailRequest) error {
	_, err := t.call(ctx, http.MethodPost, "/worker/v1/jobs/"+jobID+"/fail", req, nil)
	return err
}

func (t *HTTPTransport) Heartbeat(ctx context.Context, req api.HeartbeatRequest) error {
	_, err := t.call(ctx, http.MethodPost, "/worker/v1/heartbeat", req, nil)
	return err
}

// Register exchanges an enrollment token for the permanent worker
// credential.
func Register(ctx context.Context, client *http.Client, baseURL, token, version string) (*api.RegisterResponse, error) {
	var resp api.RegisterResponse
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := doJSON(ctx, client, http.MethodPost, baseURL+"/worker/v1/register", "",
		api.RegisterRequest{Token: token, Version: version}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
