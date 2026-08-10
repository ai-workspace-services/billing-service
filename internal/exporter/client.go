package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"billing-service/internal/config"
	"billing-service/internal/model"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Client struct {
	serviceToken string
}

func NewClient(serviceToken string) *Client {
	return &Client{
		serviceToken: strings.TrimSpace(serviceToken),
	}
}

func (c *Client) FetchWindow(ctx context.Context, source config.ExporterSource, since, until time.Time, limit int, cursor *time.Time) (model.SnapshotWindowPage, error) {
	timeout := time.Duration(source.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: otelhttp.NewTransport(http.DefaultTransport)}

	endpoint, err := url.JoinPath(strings.TrimRight(strings.TrimSpace(source.BaseURL), "/"), "/v1/snapshots/window")
	if err != nil {
		return model.SnapshotWindowPage{}, fmt.Errorf("build snapshots window endpoint: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return model.SnapshotWindowPage{}, fmt.Errorf("build snapshots window request: %w", err)
	}

	query := req.URL.Query()
	query.Set("since", since.UTC().Format(time.RFC3339))
	query.Set("until", until.UTC().Format(time.RFC3339))
	query.Set("limit", strconv.Itoa(limit))
	if cursor != nil {
		query.Set("cursor", cursor.UTC().Format(time.RFC3339))
	}
	req.URL.RawQuery = query.Encode()
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.serviceToken)

	resp, err := client.Do(req)
	if err != nil {
		return model.SnapshotWindowPage{}, fmt.Errorf("fetch snapshots window for %s: %w", source.SourceID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return model.SnapshotWindowPage{}, fmt.Errorf("fetch snapshots window for %s: unexpected status %s", source.SourceID, resp.Status)
	}

	var page model.SnapshotWindowPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return model.SnapshotWindowPage{}, fmt.Errorf("decode snapshots window for %s: %w", source.SourceID, err)
	}
	return page, nil
}
