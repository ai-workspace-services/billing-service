package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIngestSnapshotRequiresInternalServiceToken(t *testing.T) {
	handler := New(nil, "shared-token").Routes()

	request := httptest.NewRequest(http.MethodPost, "/v1/ingest/snapshots", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer token, got %d", response.Code)
	}
}

// The ingest path sits behind Vector's http sink, whose framing for the json
// codec is a property of the fan-out hop rather than of our contract. Before
// this was tolerated, a batched (array) body made every push fail with an
// opaque 400 and no usage ever reached the ledger.
func TestDecodeSnapshotsAcceptsBothVectorFramings(t *testing.T) {
	const bare = `{"collected_at":"2026-08-03T10:00:00Z","node_id":"agent-proxy","env":"uat",
		"samples":[{"uuid":"11111111-1111-1111-1111-111111111111","inbound_tag":"xhttp",
		"uplink_bytes_total":100,"downlink_bytes_total":50}]}`

	cases := []struct {
		name      string
		body      string
		wantCount int
		wantNode  string
	}{
		{
			name:      "bare object as posted by xray-exporter",
			body:      bare,
			wantCount: 1,
			wantNode:  "agent-proxy",
		},
		{
			name:      "single-element array as emitted by a batching http sink",
			body:      "[" + bare + "]",
			wantCount: 1,
			wantNode:  "agent-proxy",
		},
		{
			name:      "multi-element array",
			body:      "[" + bare + "," + bare + "]",
			wantCount: 2,
			wantNode:  "agent-proxy",
		},
		{
			name:      "newline delimited objects",
			body:      bare + "\n" + bare,
			wantCount: 2,
			wantNode:  "agent-proxy",
		},
		{
			name: "object carrying Vector's own event metadata",
			// Vector's http_server source stamps events with fields of its
			// own; unknown keys must not make the snapshot undecodable.
			body: `{"collected_at":"2026-08-03T10:00:00Z","node_id":"agent-proxy","env":"uat",
				"samples":[],"timestamp":"2026-08-03T10:00:01Z","source_type":"http_server","path":"/"}`,
			wantCount: 1,
			wantNode:  "agent-proxy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshots, err := decodeSnapshots([]byte(tc.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(snapshots) != tc.wantCount {
				t.Fatalf("expected %d snapshot(s), got %d", tc.wantCount, len(snapshots))
			}
			for i, snapshot := range snapshots {
				if snapshot.NodeID != tc.wantNode {
					t.Fatalf("snapshot %d: expected node_id %q, got %q", i, tc.wantNode, snapshot.NodeID)
				}
				if snapshot.Env != "uat" {
					t.Fatalf("snapshot %d: expected env uat, got %q", i, snapshot.Env)
				}
				if snapshot.CollectedAt.IsZero() {
					t.Fatalf("snapshot %d: collected_at did not survive decoding", i)
				}
			}
		})
	}
}

func TestDecodeSnapshotsPreservesSampleTotals(t *testing.T) {
	body := `[{"collected_at":"2026-08-03T10:00:00Z","node_id":"n","env":"uat","samples":[
		{"uuid":"11111111-1111-1111-1111-111111111111","inbound_tag":"xhttp","uplink_bytes_total":100,"downlink_bytes_total":50},
		{"uuid":"11111111-1111-1111-1111-111111111111","inbound_tag":"tcp","uplink_bytes_total":30,"downlink_bytes_total":20}]}]`

	snapshots, err := decodeSnapshots([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snapshots) != 1 || len(snapshots[0].Samples) != 2 {
		t.Fatalf("expected 1 snapshot with 2 samples, got %#v", snapshots)
	}
	// Per-inbound samples must arrive intact; aggregation by UUID happens in
	// the service layer, not here.
	if got := snapshots[0].Samples[0].UplinkBytesTotal; got != 100 {
		t.Fatalf("expected first sample uplink 100, got %d", got)
	}
	if got := snapshots[0].Samples[1].UplinkBytesTotal; got != 30 {
		t.Fatalf("expected second sample uplink 30, got %d", got)
	}
}

func TestDecodeSnapshotsRejectsUndecodableBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "whitespace only", body: "  \n "},
		{name: "empty array", body: "[]"},
		{name: "malformed json", body: `{"collected_at":`},
		{name: "wrong root type", body: `"just-a-string"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeSnapshots([]byte(tc.body)); err == nil {
				t.Fatalf("expected an error for %q", tc.body)
			}
		})
	}
}

// A rejected push previously returned a bare "invalid snapshot", leaving the
// sender with no way to tell which encoding it had produced. The response now
// carries a bounded excerpt of the body.
func TestIngestSnapshotErrorIncludesBodyExcerpt(t *testing.T) {
	handler := New(nil, "shared-token").Routes()

	request := httptest.NewRequest(http.MethodPost, "/v1/ingest/snapshots",
		strings.NewReader(`{"collected_at":`))
	request.Header.Set("Authorization", "Bearer shared-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed body, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "body prefix:") {
		t.Fatalf("expected the error to quote the received body, got %q", body)
	}
	if !strings.Contains(body, `{"collected_at":`) {
		t.Fatalf("expected the excerpt to show what arrived, got %q", body)
	}
}

func TestBodyPrefixIsBounded(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := bodyPrefix([]byte(long))
	if len(got) > 130 {
		t.Fatalf("expected a bounded excerpt, got %d chars", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncation marker, got %q", got[len(got)-10:])
	}
}
