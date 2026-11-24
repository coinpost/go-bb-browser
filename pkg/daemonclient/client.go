package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/coinpost/go-bb-browser/pkg/protocol"
)

// Client talks to bb-daemon over HTTP (JSON-RPC POST /v1).
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Headers, if non-empty, are applied to every Health and Call request via [http.Header.Set]
	// (same key with multiple stored values keeps the last in slice order).
	// WithHeader / WithHeaders merge into this map via [http.Header.Add] (they do not replace each other).
	Headers http.Header

	id atomic.Uint64
}

// ClientOption configures a [Client] when passed to [NewClient].
type ClientOption func(*Client)

// WithAPIToken adds one API token, equivalent to WithHeader("Authorization", "Bearer "+token).
// Each client in a Pool can use its own token.
// Surrounding whitespace is trimmed; an empty token adds no header.
func WithAPIToken(token string) ClientOption {
	token = strings.TrimSpace(token)
	if token == "" {
		return func(*Client) {}
	}
	return WithHeader("Authorization", "Bearer "+token)
}

// WithHeader returns a [ClientOption] that merges a single header via [http.Header.Add].
func WithHeader(k, v string) ClientOption {
	return func(c *Client) {
		if c.Headers == nil {
			c.Headers = make(http.Header)
		}
		c.Headers.Add(k, v)
	}
}

// WithHeaders returns a [ClientOption] that merges all entries from h via [http.Header.Add]
// (including when combined with [WithHeader] or multiple WithHeaders; later options append).
func WithHeaders(h http.Header) ClientOption {
	return func(c *Client) {
		if len(h) == 0 {
			return
		}
		if c.Headers == nil {
			c.Headers = make(http.Header)
		}
		for key, vals := range h {
			for _, val := range vals {
				c.Headers.Add(key, val)
			}
		}
	}
}

// NewClient returns a client for the given daemon root URL (e.g. http://127.0.0.1:8080).
func NewClient(baseURL string, opts ...ClientOption) *Client {
	c := &Client{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) applyHeaders(req *http.Request) {
	applyHeaders(req, c.Headers)
}

func applyHeaders(req *http.Request, h http.Header) {
	for k, vv := range h {
		for _, v := range vv {
			req.Header.Set(k, v)
		}
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) nextID() json.RawMessage {
	n := c.id.Add(1)
	b, err := json.Marshal(n)
	if err != nil {
		// uint64 always marshals
		return json.RawMessage("1")
	}
	return json.RawMessage(b)
}

// Health checks GET /health. The daemon must report browser connectivity (HTTP 200).
func (c *Client) Health(ctx context.Context) error {
	_, err := c.HealthResult(ctx)
	return err
}

// HealthResult performs GET /health and decodes the response body.
func (c *Client) HealthResult(ctx context.Context) (protocol.HealthResult, error) {
	return c.browserStatusResult(ctx, "/health")
}

// Ready checks the cached CDP watchdog readiness exposed by GET /ready.
func (c *Client) Ready(ctx context.Context) error {
	_, err := c.ReadyResult(ctx)
	return err
}

func (c *Client) ReadyResult(ctx context.Context) (protocol.HealthResult, error) {
	return c.browserStatusResult(ctx, "/ready")
}

func (c *Client) browserStatusResult(ctx context.Context, path string) (protocol.HealthResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return protocol.HealthResult{}, err
	}
	c.applyHeaders(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return protocol.HealthResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return protocol.HealthResult{}, err
	}
	if resp.StatusCode != http.StatusOK {
		var out protocol.HealthResult
		_ = json.Unmarshal(b, &out)
		return out, &HTTPError{StatusCode: resp.StatusCode, Body: string(b)}
	}
	var out protocol.HealthResult
	if err := json.Unmarshal(b, &out); err != nil {
		return protocol.HealthResult{}, fmt.Errorf("decode health response: %w", err)
	}
	return out, nil
}

// Live checks the daemon HTTP process without consulting Chrome or CDP state.
func (c *Client) Live(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/live", nil)
	if err != nil {
		return err
	}
	c.applyHeaders(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(b)}
	}
	var out protocol.LivenessResult
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("decode liveness response: %w", err)
	}
	if out.Status != "ok" {
		return fmt.Errorf("daemon is not live: status %q", out.Status)
	}
	return nil
}

// Call performs a single JSON-RPC request on POST /v1. result must be a non-nil pointer
// to decode the result field on success; if nil, the result payload is ignored when there is no error.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.rpcTarget(false).call(ctx, method, paramsJSON, result)
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	return b, nil
}

// rpcTarget is what one POST /v1 needs from a [Client], read at a single point
// in time: where to send it, with which HTTP client and headers, and the
// request id already drawn from the Client's counter. A request made from it
// never touches the Client again, so it may outlive the call that created it.
type rpcTarget struct {
	http    *http.Client
	url     string
	headers http.Header
	id      json.RawMessage
}

// rpcTarget captures the Client's current configuration and the next request
// id. With cloneHeaders the headers are deep-copied, for requests that keep
// running on another goroutine while the caller is free to change the Client.
func (c *Client) rpcTarget(cloneHeaders bool) rpcTarget {
	t := rpcTarget{
		http:    c.httpClient(),
		url:     c.BaseURL + "/v1",
		headers: c.Headers,
		id:      c.nextID(),
	}
	if cloneHeaders {
		t.headers = c.Headers.Clone()
	}
	return t
}

func (t rpcTarget) call(ctx context.Context, method string, paramsJSON json.RawMessage, result any) error {
	reqObj := protocol.Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsJSON,
		ID:      t.id,
	}
	raw, err := json.Marshal(reqObj)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	applyHeaders(req, t.headers)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var env protocol.Response
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode json-rpc response: %w", err)
	}
	if env.Error != nil {
		return rpcErrorFrom(env.Error)
	}
	if result == nil {
		return nil
	}
	if len(env.Result) == 0 {
		return fmt.Errorf("json-rpc: missing result")
	}
	return json.Unmarshal(env.Result, result)
}

// --- Typed RPC methods ---

func (c *Client) TabList(ctx context.Context, p protocol.TabListParams) (protocol.TabListResult, error) {
	return callTyped[protocol.TabListParams, protocol.TabListResult](ctx, c, protocol.MethodTabList, p)
}

func (c *Client) TabFocus(ctx context.Context, p protocol.TabFocusParams) (protocol.TabFocusResult, error) {
	return callTyped[protocol.TabFocusParams, protocol.TabFocusResult](ctx, c, protocol.MethodTabFocus, p)
}

func (c *Client) TabSelect(ctx context.Context, p protocol.TabSelectParams) (protocol.TabSelectResult, error) {
	return callTyped[protocol.TabSelectParams, protocol.TabSelectResult](ctx, c, protocol.MethodTabSelect, p)
}

func (c *Client) TabNew(ctx context.Context, p protocol.TabNewParams) (protocol.TabNewResult, error) {
	return callTyped[protocol.TabNewParams, protocol.TabNewResult](ctx, c, protocol.MethodTabNew, p)
}

func (c *Client) Goto(ctx context.Context, p protocol.GotoParams) (protocol.GotoResult, error) {
	return callTyped[protocol.GotoParams, protocol.GotoResult](ctx, c, protocol.MethodGoto, p)
}

func (c *Client) Reload(ctx context.Context, p protocol.ReloadParams) (protocol.ReloadResult, error) {
	return callTyped[protocol.ReloadParams, protocol.ReloadResult](ctx, c, protocol.MethodReload, p)
}

func (c *Client) TabClose(ctx context.Context, p protocol.TabCloseParams) (protocol.TabCloseResult, error) {
	return callTyped[protocol.TabCloseParams, protocol.TabCloseResult](ctx, c, protocol.MethodTabClose, p)
}

func (c *Client) Screenshot(ctx context.Context, p protocol.ScreenshotParams) (protocol.ScreenshotResult, error) {
	return callTyped[protocol.ScreenshotParams, protocol.ScreenshotResult](ctx, c, protocol.MethodScreenshot, p)
}

func (c *Client) Eval(ctx context.Context, p protocol.EvalParams) (protocol.EvalResult, error) {
	return callTyped[protocol.EvalParams, protocol.EvalResult](ctx, c, protocol.MethodEval, p)
}

func (c *Client) Click(ctx context.Context, p protocol.ClickParams) (protocol.ClickResult, error) {
	return callTyped[protocol.ClickParams, protocol.ClickResult](ctx, c, protocol.MethodClick, p)
}

func (c *Client) Fill(ctx context.Context, p protocol.FillParams) (protocol.FillResult, error) {
	return callTyped[protocol.FillParams, protocol.FillResult](ctx, c, protocol.MethodFill, p)
}

func (c *Client) Network(ctx context.Context, p protocol.ObsQueryParams) (protocol.ObsQueryResult, error) {
	return callTyped[protocol.ObsQueryParams, protocol.ObsQueryResult](ctx, c, protocol.MethodNetwork, p)
}

func (c *Client) Console(ctx context.Context, p protocol.ObsQueryParams) (protocol.ObsQueryResult, error) {
	return callTyped[protocol.ObsQueryParams, protocol.ObsQueryResult](ctx, c, protocol.MethodConsole, p)
}

func (c *Client) Errors(ctx context.Context, p protocol.ObsQueryParams) (protocol.ObsQueryResult, error) {
	return callTyped[protocol.ObsQueryParams, protocol.ObsQueryResult](ctx, c, protocol.MethodErrors, p)
}

func (c *Client) Fetch(ctx context.Context, p protocol.FetchParams) (protocol.FetchResult, error) {
	return callTyped[protocol.FetchParams, protocol.FetchResult](ctx, c, protocol.MethodFetch, p)
}

func (c *Client) Snapshot(ctx context.Context, p protocol.SnapshotParams) (protocol.SnapshotResult, error) {
	return callTyped[protocol.SnapshotParams, protocol.SnapshotResult](ctx, c, protocol.MethodSnapshot, p)
}

func (c *Client) NetworkRoute(ctx context.Context, p protocol.NetworkRouteParams) (protocol.NetworkRouteResult, error) {
	return callTyped[protocol.NetworkRouteParams, protocol.NetworkRouteResult](ctx, c, protocol.MethodNetworkRoute, p)
}

func (c *Client) NetworkUnroute(ctx context.Context, p protocol.NetworkUnrouteParams) (protocol.NetworkUnrouteResult, error) {
	return callTyped[protocol.NetworkUnrouteParams, protocol.NetworkUnrouteResult](ctx, c, protocol.MethodNetworkUnroute, p)
}

func (c *Client) NetworkClear(ctx context.Context, p protocol.NetworkClearParams) (protocol.NetworkClearResult, error) {
	return callTyped[protocol.NetworkClearParams, protocol.NetworkClearResult](ctx, c, protocol.MethodNetworkClear, p)
}

func (c *Client) ConsoleClear(ctx context.Context, p protocol.ConsoleClearParams) (protocol.ConsoleClearResult, error) {
	return callTyped[protocol.ConsoleClearParams, protocol.ConsoleClearResult](ctx, c, protocol.MethodConsoleClear, p)
}

func (c *Client) ErrorsClear(ctx context.Context, p protocol.ErrorsClearParams) (protocol.ErrorsClearResult, error) {
	return callTyped[protocol.ErrorsClearParams, protocol.ErrorsClearResult](ctx, c, protocol.MethodErrorsClear, p)
}
