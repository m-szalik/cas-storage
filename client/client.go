// Package client is the HTTP client for cas-storage:
// put, fetch and delete immutable objects.
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
	base string
	http *http.Client
}

// New returns a client for baseURL (e.g. http://cas-storage:8080). The HTTP
// client has no total timeout: it would cut off OpenObject's streamed body and
// PutStream's arbitrarily large upload (a whole archived document) after 60s. Instead only the connection phases are bounded; per-call contexts remain
// the cancellation mechanism.
func New(baseURL string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
			},
		},
	}
}

// PutObject stores body and returns its content hash.
func (c *Client) PutObject(ctx context.Context, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/objects", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
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
	resp, err := c.http.Do(req)
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
	resp, err := c.http.Do(req)
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
	resp, err := c.http.Do(req)
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
	resp, err := c.http.Do(req)
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
	resp, err := c.http.Do(req)
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
