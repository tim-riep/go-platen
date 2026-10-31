// Package netbirdapi is a minimal client for the NetBird management REST
// API, as documented at https://docs.netbird.io/api. It reads peers and
// users to find out which NetBird user a device belongs to.
package netbirdapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is the API of NetBird's cloud service.
const DefaultURL = "https://api.netbird.io"

// Client calls the NetBird management API with an access token.
type Client struct {
	// BaseURL is the API's base URL without the /api path, e.g.
	// https://netbird.example.com.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Peer is a NetBird peer (device).
type Peer struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	IP        string    `json:"ip"`
	DNSLabel  string    `json:"dns_label"`
	UserID    string    `json:"user_id"`
	Hostname  string    `json:"hostname"`
	OS        string    `json:"os"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"last_seen"`
}

// User is a NetBird user.
type User struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	IsServiceUser bool   `json:"is_service_user"`
	IsBlocked     bool   `json:"is_blocked"`
}

// BaseURLFor derives the API URL from a NetBird management URL: the
// management server serves the API on the same host.
func BaseURLFor(managementURL string) string {
	if managementURL == "" {
		return DefaultURL
	}
	u, err := url.Parse(managementURL)
	if err != nil || u.Host == "" {
		return DefaultURL
	}
	host := u.Hostname()
	if host == "api.wiretrustee.com" || host == "api.netbird.io" {
		return DefaultURL
	}
	if u.Port() == "443" || u.Port() == "" {
		return "https://" + host
	}
	return "https://" + u.Host
}

// Error is an error response of the API.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("netbird api: %s", http.StatusText(e.Status))
	}
	return fmt.Sprintf("netbird api: %s: %s", http.StatusText(e.Status), e.Message)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.BaseURL, "/")+"/api"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.Token)
	req.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &e)
		return &Error{Status: resp.StatusCode, Message: e.Message}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// Peers lists all peers the token may see.
func (c *Client) Peers(ctx context.Context) ([]Peer, error) {
	var out []Peer
	return out, c.get(ctx, "/peers", &out)
}

// Users lists all users the token may see.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out []User
	return out, c.get(ctx, "/users", &out)
}
