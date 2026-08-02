package httpapi

import (
	"net/http"
	"net/http/httptest"
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
