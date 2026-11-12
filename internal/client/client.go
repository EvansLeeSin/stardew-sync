// Package client is a Go HTTP client for the stardew-sync server.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/EvansLeeSin/stardew-sync/internal/api"
	"github.com/EvansLeeSin/stardew-sync/internal/savefmt"
)

var (
	ErrNotFound = errors.New("not found")
	ErrLocked   = errors.New("locked by another device")
)

// ConflictError is returned when upload base is stale.
type ConflictError struct{ Latest *api.Revision }

func (e *ConflictError) Error() string {
	if e.Latest == nil {
		return "conflict"
	}
	return fmt.Sprintf("conflict: server has rev %d from %s", e.Latest.Rev, e.Latest.Device)
}

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 0},
	}
}

func (c *Client) url(id string, parts ...string) string {
	u := c.BaseURL + "/api/v1/saves"
	if id != "" {
		u += "/" + url.PathEscape(id)
	}
	for _, p := range parts {
		u += "/" + p
	}
	return u
}

func (c *Client) do(ctx context.Context, method, u string, body io.Reader, ctype string, timeout time.Duration) (*http.Response, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		resp, err := c.doCtx(ctx, method, u, body, ctype)
		if err != nil {
			cancel()
			return nil, err
		}
		resp.Body = &cancelBody{resp.Body, cancel}
		return resp, nil
	}
	return c.doCtx(ctx, method, u, body, ctype)
}

func (c *Client) doCtx(ctx context.Context, method, u string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "stardew-sync-go/1")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return c.HTTP.Do(req)
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { err := b.ReadCloser.Close(); b.cancel(); return err }

func decode(resp *http.Response, v any) error {
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(b, v)
}

func httpErr(resp *http.Response) error {
	defer resp.Body.Close()
	var e api.ErrorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("server %d: %s", resp.StatusCode, e.Error)
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200]
	}
	return fmt.Errorf("server %d: %s", resp.StatusCode, s)
}

// WhoAmI returns the device name for the token.
func (c *Client) WhoAmI(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, "GET", c.BaseURL+"/api/v1/whoami", nil, "", 20*time.Second)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", httpErr(resp)
	}
	var v struct{ Device string }
	return v.Device, decode(resp, &v)
}

// Latest returns the latest revision; ErrNotFound if none.
func (c *Client) Latest(ctx context.Context, id string) (*api.SaveSummary, error) {
	resp, err := c.do(ctx, "GET", c.url(id), nil, "", 30*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 404 {
		resp.Body.Close()
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	var s api.SaveSummary
	return &s, decode(resp, &s)
}

// Revisions lists revisions newest first.
func (c *Client) Revisions(ctx context.Context, id string) ([]api.Revision, error) {
	resp, err := c.do(ctx, "GET", c.url(id, "revisions"), nil, "", 30*time.Second)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	var out []api.Revision
	return out, decode(resp, &out)
}

// Download fetches a revision zip.
func (c *Client) Download(ctx context.Context, id string, rev int) ([]byte, error) {
	resp, err := c.do(ctx, "GET", c.url(id, "revisions", strconv.Itoa(rev), "archive"), nil, "", 5*time.Minute)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, httpErr(resp)
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, savefmt.MaxArchiveSize+1))
}

// Upload uploads a save zip.
func (c *Client) Upload(ctx context.Context, id string, data []byte, base int, force bool) (*api.UploadResult, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	m, _ := json.Marshal(api.UploadMeta{BaseRevision: base, Force: force})
	mw.WriteField("meta", string(m))
	fw, _ := mw.CreateFormFile("archive", id+".zip")
	fw.Write(data)
	mw.Close()
	resp, err := c.do(ctx, "POST", c.url(id, "revisions"), &buf, mw.FormDataContentType(), 5*time.Minute)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case 200, 201:
		var r api.UploadResult
		return &r, decode(resp, &r)
	case 409:
		var ce api.ConflictError
		decode(resp, &ce)
		return nil, &ConflictError{Latest: ce.Latest}
	default:
		return nil, httpErr(resp)
	}
}

// Restore copies an old revision as a new latest revision.
func (c *Client) Restore(ctx context.Context, id string, rev int) (*api.UploadResult, error) {
	resp, err := c.do(ctx, "POST", c.url(id, "restore", strconv.Itoa(rev)), nil, "", time.Minute)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, httpErr(resp)
	}
	var r api.UploadResult
	return &r, decode(resp, &r)
}

// Wait long-polls until a revision newer than after exists. Returns
// (summary, true) on change, (nil, false) on timeout.
func (c *Client) Wait(ctx context.Context, id string, after int, timeout time.Duration) (*api.SaveSummary, bool, error) {
	u := fmt.Sprintf("%s?after=%d&timeout=%d", c.url(id, "wait"), after, int(timeout.Seconds()))
	resp, err := c.do(ctx, "GET", u, nil, "", timeout+20*time.Second)
	if err != nil {
		return nil, false, err
	}
	switch resp.StatusCode {
	case 204:
		resp.Body.Close()
		return nil, false, nil
	case 200:
		var s api.SaveSummary
		return &s, true, decode(resp, &s)
	default:
		return nil, false, httpErr(resp)
	}
}

// Lock acquires/renews the play lock. On 423 returns the current lock and ErrLocked.
func (c *Client) Lock(ctx context.Context, id string, force bool, gameDate string) (*api.Lock, error) {
	b, _ := json.Marshal(api.LockRequest{Force: force, GameDate: gameDate})
	resp, err := c.do(ctx, "PUT", c.url(id, "lock"), bytes.NewReader(b), "application/json", 20*time.Second)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case 200, 423:
		var lr api.LockResponse
		if err := decode(resp, &lr); err != nil {
			return nil, err
		}
		if !lr.Acquired {
			return lr.Lock, ErrLocked
		}
		return lr.Lock, nil
	default:
		return nil, httpErr(resp)
	}
}

// Unlock releases the play lock (best effort).
func (c *Client) Unlock(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "DELETE", c.url(id, "lock"), nil, "", 20*time.Second)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 423 {
		return fmt.Errorf("unlock: %d", resp.StatusCode)
	}
	return nil
}
