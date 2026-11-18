// Package client is the HTTP client for cas-storage:
// put, fetch and delete immutable objects and manage their labels.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/m-szalik/goutils"

	"github.com/m-szalik/cas-storage/cas"
)

// Client talks to a cas-storage instance.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sends key as X-API-Key on every request.
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// New returns a client for baseURL (e.g. http://cas-storage:8080). The HTTP
// client has no total timeout: it would cut off OpenObject's streamed body and
// PutStream's arbitrarily large upload (a whole archived document) after 60s. Instead only the connection phases are bounded; per-call contexts remain
// the cancellation mechanism.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
			},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do sends req with the API key.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return c.http.Do(req)
}

// PutObject stores body and returns its content hash.
func (c *Client) PutObject(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/objects", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer goutils.CloseQuietly(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("storage: put object: %s", resp.Status)
	}
	var res cas.PutResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("storage: decode put result: %w", err)
	}
	return res.Hash, nil
}

// GetObject fetches the object stored under hash. It returns an error when the
// object is absent or the request fails.
func (c *Client) GetObject(ctx context.Context, hash string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/objects/"+hash, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer goutils.CloseQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage: get object %s: %s", hash, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// HeadObject reports whether the object under hash exists. A 404 returns
// (false, nil); any other non-2xx status is an error.
func (c *Client) HeadObject(ctx context.Context, hash string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.base+"/objects/"+hash, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer goutils.CloseQuietly(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("storage: head object %s: %s", hash, resp.Status)
	}
}

// DeleteObject removes the object under hash unless it was put at or after
// unusedBefore. It answers the object's size and whether it was removed; an
// absent object is (0, false, nil). The storage must allow deletes
// (STORAGE_ALLOW_DELETE).
func (c *Client) DeleteObject(ctx context.Context, hash string, unusedBefore time.Time) (size int64, deleted bool, err error) {
	u := c.base + "/objects/" + hash + "?unused_before=" + url.QueryEscape(unusedBefore.UTC().Format(time.RFC3339))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, false, err
	}
	defer goutils.CloseQuietly(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("storage: delete object %s: %s", hash, resp.Status)
	}
	var res cas.DeleteResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return 0, false, fmt.Errorf("storage: decode delete result: %w", err)
	}
	return res.Size, res.Deleted, nil
}

// OpenObject streams the object under hash. The caller must close the returned
// reader. It returns an error when the object is absent or the request fails.
func (c *Client) OpenObject(ctx context.Context, hash string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/objects/"+hash, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		goutils.CloseQuietly(resp.Body)
		return nil, fmt.Errorf("storage: get object %s: %s", hash, resp.Status)
	}
	return resp.Body, nil
}

// PutStream stores the bytes read from r and returns the content hash. Unlike
// PutObject it does not buffer the whole object in memory, so it can mirror a
// large document straight from another storage's GET body.
func (c *Client) PutStream(ctx context.Context, r io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/objects", r)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer goutils.CloseQuietly(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("storage: put object: %s", resp.Status)
	}
	var res cas.PutResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return "", fmt.Errorf("storage: decode put result: %w", err)
	}
	return res.Hash, nil
}

// Labels returns the labels of the object under hash. It returns an error when
// the object is absent or the request fails.
func (c *Client) Labels(ctx context.Context, hash string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/objects/"+hash+"/labels", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer goutils.CloseQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage: labels %s: %s", hash, resp.Status)
	}
	var labels map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&labels); err != nil {
		return nil, fmt.Errorf("storage: decode labels: %w", err)
	}
	return labels, nil
}

// SetLabel sets the label key to value on the object under hash.
func (c *Client) SetLabel(ctx context.Context, hash, key, value string) error {
	return c.label(ctx, http.MethodPut, hash, key, strings.NewReader(value))
}

// DeleteLabel removes the label key from the object under hash; removing a
// label it does not have is no error.
func (c *Client) DeleteLabel(ctx context.Context, hash, key string) error {
	return c.label(ctx, http.MethodDelete, hash, key, nil)
}

// label sends a PUT or DELETE to /objects/{hash}/labels/{key}.
func (c *Client) label(ctx context.Context, method, hash, key string, body io.Reader) error {
	u := c.base + "/objects/" + hash + "/labels/" + url.PathEscape(key)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer goutils.CloseQuietly(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("storage: %s label %s on %s: %s", strings.ToLower(method), key, hash, resp.Status)
	}
	return nil
}

// FindByLabels returns, sorted, the hashes of the objects that carry every
// label in sel; an empty value matches any value of that key.
func (c *Client) FindByLabels(ctx context.Context, sel map[string]string) ([]string, error) {
	q := url.Values{}
	for k, v := range sel {
		if v == "" {
			q.Add("label", k)
		} else {
			q.Add("label", k+"="+v)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/objects?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer goutils.CloseQuietly(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("storage: find by labels: %s", resp.Status)
	}
	var res cas.FindResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("storage: decode find result: %w", err)
	}
	return res.Hashes, nil
}
