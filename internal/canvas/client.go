package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	token      string
	cookies    []*http.Cookie // session cookie auth (for SNU myETL)
	httpClient *http.Client
	logger     *slog.Logger
}

func NewClient(baseURL, token string, logger *slog.Logger) *Client {
	if !strings.HasPrefix(baseURL, "http") {
		baseURL = "https://" + baseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger: logger,
	}
}

// SetCookies sets session cookies for authentication (used when Bearer token
// auth is unavailable, e.g. SNU myETL which requires session-based auth).
func (c *Client) SetCookies(cookies []*http.Cookie) {
	c.cookies = cookies
}

// addAuth adds authentication to a request. Prefers session cookies if set,
// otherwise falls back to Bearer token.
func (c *Client) addAuth(req *http.Request) {
	if len(c.cookies) > 0 {
		for _, ck := range c.cookies {
			req.AddCookie(ck)
		}
	} else if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// stripJSONPrefix removes the "while(1);" CSRF prefix that some Canvas
// instances (e.g. SNU myETL) prepend to JSON responses.
func stripJSONPrefix(data []byte) []byte {
	if bytes.HasPrefix(data, []byte("while(1);")) {
		return data[9:]
	}
	return data
}

func (c *Client) do(ctx context.Context, method, path string, params url.Values) (*http.Response, error) {
	u := c.baseURL + "/api/v1" + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	c.addAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}

	if resp.StatusCode == 429 {
		resp.Body.Close()
		c.logger.Warn("rate limited, waiting 10s")
		time.Sleep(10 * time.Second)
		return c.do(ctx, method, path, params)
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Wrap body to strip "while(1);" CSRF prefix if present
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(stripJSONPrefix(body)))

	return resp, nil
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (c *Client) getAll(ctx context.Context, path string, params url.Values, target interface{}) error {
	resp, err := c.do(ctx, http.MethodGet, path, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	return nil
}

// getPaginated fetches all pages and returns raw JSON arrays concatenated.
func (c *Client) getPaginated(ctx context.Context, path string, params url.Values) ([]json.RawMessage, error) {
	var all []json.RawMessage

	u := c.baseURL + "/api/v1" + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}

	for u != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		c.addAuth(req)
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == 429 {
			resp.Body.Close()
			c.logger.Warn("rate limited, waiting 10s")
			time.Sleep(10 * time.Second)
			continue
		}

		if resp.StatusCode >= 400 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, body)
		}

		rawBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		var page []json.RawMessage
		if err := json.Unmarshal(stripJSONPrefix(rawBody), &page); err != nil {
			return nil, err
		}
		all = append(all, page...)

		// Parse Link header for next page
		u = ""
		if link := resp.Header.Get("Link"); link != "" {
			if m := linkNextRe.FindStringSubmatch(link); len(m) > 1 {
				u = m[1]
			}
		}
	}

	return all, nil
}

// DownloadFile downloads a file and returns its content.
func (c *Client) DownloadFile(ctx context.Context, fileURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, err
	}
	c.addAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download error %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
