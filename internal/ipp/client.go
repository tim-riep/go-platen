package ipp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ContentType is the media type of IPP messages over HTTP.
const ContentType = "application/ipp"

// HTTPURL turns an ipp:// or ipps:// printer URI into the http:// or
// https:// URL the request is posted to. The default port is 631 for both.
func HTTPURL(printerURI string) (string, error) {
	u, err := url.Parse(printerURI)
	if err != nil {
		return "", fmt.Errorf("ipp: printer uri: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "ipp", "http":
		u.Scheme = "http"
	case "ipps", "https":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("ipp: printer uri %q: scheme must be ipp or ipps", printerURI)
	}
	if u.Host == "" {
		return "", fmt.Errorf("ipp: printer uri %q has no host", printerURI)
	}
	if u.Port() == "" {
		u.Host += ":631"
	}
	return u.String(), nil
}

// Post sends an encoded request, optionally followed by a document, to
// the printer and decodes the response. size is the length of body, or -1
// if unknown (then the request is sent chunked).
func Post(ctx context.Context, client *http.Client, printerURI string, body io.Reader, size int64) (*Message, error) {
	target, err := HTTPURL(printerURI)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set("Accept", ContentType)
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: res.StatusCode}
	}
	m, err := Decode(bufio.NewReader(io.LimitReader(res.Body, 16<<20)))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// Do sends a request without a document.
func Do(ctx context.Context, client *http.Client, printerURI string, req *Message) (*Message, error) {
	data := req.Encode()
	return Post(ctx, client, printerURI, bytes.NewReader(data), int64(len(data)))
}

// HTTPError is a response with an HTTP status other than 200.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("ipp: http %d %s", e.Status, http.StatusText(e.Status))
}
