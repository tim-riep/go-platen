// Package admin implements the "platen admin" commands. They call the admin
// API of the server; the binary routes the requests through NetBird.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tim-riep/go-platen/internal/api"
)

// Client calls the admin API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var eb api.ErrorBody
		if json.Unmarshal(b, &eb) == nil && eb.Error != nil {
			eb.Error.Status = resp.StatusCode
			return eb.Error
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Usage describes the admin commands.
const Usage = `Usage: platen admin -config <file> <resource> <action> [arguments]

Workers:
  worker list
  worker get <id>
  worker create <id> [-name N] [-location L]
  worker update <id> [-name N] [-location L]
  worker delete <id>
  worker enroll <id> [-ttl 24h]          create a single-use enrollment token

Printers:
  printer list [-worker W]
  printer get <id>
  printer create <id> -worker W -address HOST[:PORT] -backend B
                 [-name N] [-paper A4] [-dpi 600] [-status ready|paused]
                 [-option key=value ...]
  printer update <id> [same flags as create]
  printer delete <id>

App clients:
  client list
  client get <id>
  client create <id> [-name N] [-printers p1,p2] [-all-printers]
  client update <id> [-name N] [-printers p1,p2] [-all-printers=true|false]
  client delete <id>
  client rotate-token <id>

Jobs:
  job list [-status S] [-printer P] [-client C] [-worker W] [-limit N]
  job get <id>
  job cancel <id>

Output is JSON.
`

// ErrUsage is returned for invalid command lines.
var ErrUsage = errors.New("invalid command line")

// Run executes an admin command and writes the result to out.
func Run(ctx context.Context, c *Client, args []string, out io.Writer) error {
	if len(args) < 2 {
		return ErrUsage
	}
	resource, action, rest := args[0], args[1], args[2:]
	var result any
	var err error
	switch resource {
	case "worker", "workers":
		result, err = workerCmd(ctx, c, action, rest)
	case "printer", "printers":
		result, err = printerCmd(ctx, c, action, rest)
	case "client", "clients", "app-client":
		result, err = clientCmd(ctx, c, action, rest)
	case "job", "jobs":
		result, err = jobCmd(ctx, c, action, rest)
	default:
		return fmt.Errorf("%w: unknown resource %q", ErrUsage, resource)
	}
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// flags parses flags that may appear before or after the positional
// arguments and returns the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUsage, err)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func oneID(fs *flag.FlagSet, args []string) (string, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return "", err
	}
	if len(pos) != 1 {
		return "", fmt.Errorf("%w: expected exactly one id", ErrUsage)
	}
	return pos[0], nil
}

// set reports which flags were given on the command line.
func set(fs *flag.FlagSet) map[string]bool {
	m := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { m[f.Name] = true })
	return m
}

func ptr[T any](given bool, v T) *T {
	if !given {
		return nil
	}
	return &v
}

func path(parts ...string) string {
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/" + strings.Join(parts, "/")
}

func workerCmd(ctx context.Context, c *Client, action string, args []string) (any, error) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	name := fs.String("name", "", "display name")
	location := fs.String("location", "", "location")
	ttl := fs.Duration("ttl", 0, "enrollment token lifetime")
	switch action {
	case "list":
		if _, err := parse(fs, args); err != nil {
			return nil, err
		}
		var l api.List[api.Worker]
		return l, c.do(ctx, http.MethodGet, "/admin/v1/workers", nil, &l)
	case "get", "delete", "enroll", "create", "update":
	default:
		return nil, fmt.Errorf("%w: unknown worker action %q", ErrUsage, action)
	}
	id, err := oneID(fs, args)
	if err != nil {
		return nil, err
	}
	given := set(fs)
	p := "/admin/v1/workers/" + url.PathEscape(id)
	var w api.Worker
	switch action {
	case "get":
		return w, c.do(ctx, http.MethodGet, p, nil, &w)
	case "delete":
		return nil, c.do(ctx, http.MethodDelete, p, nil, nil)
	case "enroll":
		var t api.EnrollmentToken
		return t, c.do(ctx, http.MethodPost, p+"/enrollment-tokens", api.EnrollmentTokenRequest{TTLSeconds: int(ttl.Seconds())}, &t)
	case "create":
		req := api.WorkerRequest{ID: id, Name: ptr(given["name"], *name), Location: ptr(given["location"], *location)}
		return w, c.do(ctx, http.MethodPost, "/admin/v1/workers", req, &w)
	default: // update
		req := api.WorkerRequest{Name: ptr(given["name"], *name), Location: ptr(given["location"], *location)}
		return w, c.do(ctx, http.MethodPatch, p, req, &w)
	}
}

// options collects repeated -option key=value flags.
type options map[string]string

func (o options) String() string {
	keys := make([]string, 0, len(o))
	for k := range o {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (o options) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("option must be key=value")
	}
	o[k] = v
	return nil
}

func printerCmd(ctx context.Context, c *Client, action string, args []string) (any, error) {
	fs := flag.NewFlagSet("printer", flag.ContinueOnError)
	worker := fs.String("worker", "", "worker id")
	name := fs.String("name", "", "display name")
	address := fs.String("address", "", "host[:port] in the worker's network")
	backendName := fs.String("backend", "", "backend (raw, pdf-to-raster, ...)")
	paper := fs.String("paper", "", "paper size")
	dpi := fs.Int("dpi", 0, "resolution in dpi")
	status := fs.String("status", "", "ready or paused")
	opts := options{}
	fs.Var(opts, "option", "backend option key=value (repeatable)")
	switch action {
	case "list":
		if _, err := parse(fs, args); err != nil {
			return nil, err
		}
		q := ""
		if *worker != "" {
			q = "?worker=" + url.QueryEscape(*worker)
		}
		var l api.List[api.AdminPrinter]
		return l, c.do(ctx, http.MethodGet, "/admin/v1/printers"+q, nil, &l)
	case "get", "delete", "create", "update":
	default:
		return nil, fmt.Errorf("%w: unknown printer action %q", ErrUsage, action)
	}
	id, err := oneID(fs, args)
	if err != nil {
		return nil, err
	}
	given := set(fs)
	p := "/admin/v1/printers/" + url.PathEscape(id)
	var pr api.AdminPrinter
	req := api.PrinterRequest{
		Name: ptr(given["name"], *name), WorkerID: ptr(given["worker"], *worker), Address: ptr(given["address"], *address),
		Backend: ptr(given["backend"], *backendName), PaperSize: ptr(given["paper"], *paper),
		ResolutionDPI: ptr(given["dpi"], *dpi), Status: ptr(given["status"], *status),
	}
	if given["option"] {
		m := map[string]string(opts)
		req.Options = &m
	}
	switch action {
	case "get":
		return pr, c.do(ctx, http.MethodGet, p, nil, &pr)
	case "delete":
		return nil, c.do(ctx, http.MethodDelete, p, nil, nil)
	case "create":
		req.ID = id
		return pr, c.do(ctx, http.MethodPost, "/admin/v1/printers", req, &pr)
	default:
		return pr, c.do(ctx, http.MethodPatch, p, req, &pr)
	}
}

func clientCmd(ctx context.Context, c *Client, action string, args []string) (any, error) {
	fs := flag.NewFlagSet("client", flag.ContinueOnError)
	name := fs.String("name", "", "display name")
	printers := fs.String("printers", "", "comma separated printer ids the client may use")
	all := fs.Bool("all-printers", false, "allow all printers")
	switch action {
	case "list":
		if _, err := parse(fs, args); err != nil {
			return nil, err
		}
		var l api.List[api.AppClient]
		return l, c.do(ctx, http.MethodGet, "/admin/v1/app-clients", nil, &l)
	case "get", "delete", "create", "update", "rotate-token":
	default:
		return nil, fmt.Errorf("%w: unknown client action %q", ErrUsage, action)
	}
	id, err := oneID(fs, args)
	if err != nil {
		return nil, err
	}
	given := set(fs)
	p := "/admin/v1/app-clients/" + url.PathEscape(id)
	var ac api.AppClient
	req := api.AppClientRequest{Name: ptr(given["name"], *name), AllPrinters: ptr(given["all-printers"], *all)}
	if given["printers"] {
		list := []string{}
		for _, s := range strings.Split(*printers, ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		req.Printers = &list
	}
	switch action {
	case "get":
		return ac, c.do(ctx, http.MethodGet, p, nil, &ac)
	case "delete":
		return nil, c.do(ctx, http.MethodDelete, p, nil, nil)
	case "rotate-token":
		return ac, c.do(ctx, http.MethodPost, p+"/rotate-token", nil, &ac)
	case "create":
		req.ID = id
		return ac, c.do(ctx, http.MethodPost, "/admin/v1/app-clients", req, &ac)
	default:
		return ac, c.do(ctx, http.MethodPatch, p, req, &ac)
	}
}

func jobCmd(ctx context.Context, c *Client, action string, args []string) (any, error) {
	fs := flag.NewFlagSet("job", flag.ContinueOnError)
	status := fs.String("status", "", "filter by status")
	printer := fs.String("printer", "", "filter by printer")
	client := fs.String("client", "", "filter by app client")
	worker := fs.String("worker", "", "filter by worker (lease holder)")
	limit := fs.Int("limit", 0, "maximum number of jobs")
	switch action {
	case "list":
		if _, err := parse(fs, args); err != nil {
			return nil, err
		}
		q := url.Values{}
		for k, v := range map[string]string{"status": *status, "printer": *printer, "app_client": *client, "worker": *worker} {
			if v != "" {
				q.Set(k, v)
			}
		}
		if *limit > 0 {
			q.Set("limit", fmt.Sprint(*limit))
		}
		p := "/admin/v1/jobs"
		if len(q) > 0 {
			p += "?" + q.Encode()
		}
		var l api.List[api.AdminJob]
		return l, c.do(ctx, http.MethodGet, p, nil, &l)
	case "get", "cancel":
	default:
		return nil, fmt.Errorf("%w: unknown job action %q", ErrUsage, action)
	}
	id, err := oneID(fs, args)
	if err != nil {
		return nil, err
	}
	var j api.AdminJob
	if action == "get" {
		return j, c.do(ctx, http.MethodGet, path("admin", "v1", "jobs", id), nil, &j)
	}
	return j, c.do(ctx, http.MethodPost, path("admin", "v1", "jobs", id, "cancel"), nil, &j)
}
