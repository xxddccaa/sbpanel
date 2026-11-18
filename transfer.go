package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	routeNameProxy  = "代理"
	routeNameDirect = "直连"
)

// transferRoute is one way out to the internet: through the local proxy, or direct.
type transferRoute struct {
	name  string
	proxy *url.URL
}

// transferRoutes lists the ways to fetch something from outside: through the running
// proxy first (encrypted, and GitHub is often unreachable directly), then direct.
func (a *App) transferRoutes() []transferRoute {
	var routes []transferRoute
	if a.core.Running() {
		routes = append(routes, transferRoute{name: routeNameProxy, proxy: localProxyURL(a.store.Snapshot().Settings)})
	}
	return append(routes, transferRoute{name: routeNameDirect})
}

func newHTTPClient(proxy *url.URL, timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// fetchWithFallback GETs rawURL over each route in turn until one succeeds. consume
// reads the response body; it is called again from scratch for every attempt.
func (a *App) fetchWithFallback(ctx context.Context, rawURL string, header http.Header, timeout time.Duration,
	consume func(*http.Response) error) (string, error) {
	var failures []error
	for _, route := range a.transferRoutes() {
		err := fetchOnce(ctx, newHTTPClient(route.proxy, timeout), rawURL, header, consume)
		if err == nil {
			return route.name, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		failures = append(failures, fmt.Errorf("%s: %w", route.name, err))
	}
	return "", errors.Join(failures...)
}

func fetchOnce(ctx context.Context, client *http.Client, rawURL string, header http.Header, consume func(*http.Response) error) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	for key, values := range header {
		request.Header[key] = values
	}
	response, err := client.Do(request)
	if err != nil {
		return stripURL(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回 HTTP %d", response.StatusCode)
	}
	return consume(response)
}

// stripURL drops the request URL from net/http errors. Subscription URLs carry the
// account secret, and these messages end up in the UI, state.json and the journal.
func stripURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

// readLimited reads at most limit bytes and fails instead of truncating.
func readLimited(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, stripURL(err)
	}
	if int64(len(data)) > limit {
		return nil, errors.New("内容太大")
	}
	return data, nil
}
