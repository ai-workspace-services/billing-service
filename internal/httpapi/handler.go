package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"billing-service/internal/model"
	"billing-service/internal/observability"
	"billing-service/internal/service"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type Handler struct {
	service *service.Service
	token   string
	db      dbPinger
}

type dbPinger interface {
	PingContext(context.Context) error
}

func New(svc *service.Service, token string, db ...dbPinger) *Handler {
	h := &Handler{service: svc, token: strings.TrimSpace(token)}
	if len(db) > 0 {
		h.db = db[0]
	}
	return h
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", h.ping)
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/readyz", h.readyz)
	mux.HandleFunc("/v1/status", h.status)
	mux.HandleFunc("/v1/jobs/collect-and-rate", h.collectAndRate)
	mux.HandleFunc("/v1/jobs/reconcile", h.reconcile)
	mux.HandleFunc("/v1/ingest/snapshots", h.ingestSnapshot)
	return mux
}

func (h *Handler) ingestSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.token == "" || strings.TrimSpace(r.Header.Get("Authorization")) != "Bearer "+h.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	reqCtx := r.Context()
	if !trace.SpanContextFromContext(reqCtx).IsValid() {
		reqCtx = propagation.TraceContext{}.Extract(reqCtx, propagation.HeaderCarrier(r.Header))
	}

	_, decodeSpan := otel.Tracer("billing-service").Start(reqCtx, "billing.ingest.decode",
		trace.WithAttributes(attribute.String("stage", "decode")),
	)
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngestBodyBytes))
	decodeSpan.SetAttributes(attribute.Int("payload_bytes", len(body)))
	if err != nil {
		decodeSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "body_read_error"),
		)
		decodeSpan.SetStatus(codes.Error, "read_snapshot_body_failed")
		decodeSpan.End()
		http.Error(w, "invalid snapshot: "+err.Error(), http.StatusBadRequest)
		return
	}
	snapshots, err := decodeSnapshots(body)
	if err != nil {
		decodeSpan.SetAttributes(
			attribute.String("outcome", "error"),
			attribute.String("error_class", "invalid_json"),
		)
		decodeSpan.SetStatus(codes.Error, "invalid_snapshot_json")
		decodeSpan.End()
		// Echo a bounded prefix of what actually arrived. Without it a shape
		// mismatch upstream only ever surfaces as an opaque 400 on the sender
		// side, with no way to tell which encoding produced it.
		http.Error(w, "invalid snapshot: "+err.Error()+"; body prefix: "+bodyPrefix(body), http.StatusBadRequest)
		return
	}
	for _, snapshot := range snapshots {
		if producerSpanCtx, ok := observability.ExtractTraceParent(snapshot.ProducerTraceParent, snapshot.TraceParent); ok {
			decodeSpan.AddLink(trace.Link{SpanContext: producerSpanCtx})
		}
	}
	decodeSpan.SetAttributes(
		attribute.String("outcome", "ok"),
		attribute.Int("snapshot_count", len(snapshots)),
	)
	decodeSpan.End()

	// Report the worst outcome across the batch: one bad snapshot must not be
	// hidden by later good ones, and a partial success must not read as a
	// clean 200 to the sender.
	var (
		lastResult model.JobResult
		firstErr   error
	)
	for _, snapshot := range snapshots {
		result, err := h.service.IngestSnapshot(reqCtx, snapshot)
		lastResult = result
		if err != nil && firstErr == nil {
			firstErr = err
			lastResult = result
		}
	}
	if firstErr != nil {
		writeJSON(w, http.StatusUnprocessableEntity, lastResult)
		return
	}
	writeJSON(w, http.StatusOK, lastResult)
}

// maxIngestBodyBytes caps how much of a snapshot push is read into memory.
// Snapshots carry one sample per (uuid, inbound) pair for a single node and
// minute, so this is far above any legitimate payload.
const maxIngestBodyBytes = 32 << 20

// decodeSnapshots accepts both shapes the ingest path can legitimately see:
// a bare Snapshot object (what xray-exporter posts directly) and a JSON array
// of them (what Vector's http sink emits when it batches events). Vector's
// framing for the json codec is a deployment detail of the fan-out hop, and
// the collector should not break when it changes; newline-delimited bodies
// decode through the same streaming path.
func decodeSnapshots(body []byte) ([]model.Snapshot, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("empty body")
	}

	if trimmed[0] == '[' {
		var batch []model.Snapshot
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return nil, errors.New("empty snapshot array")
		}
		return batch, nil
	}

	// One or more concatenated/newline-delimited objects.
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var snapshots []model.Snapshot
	for {
		var snapshot model.Snapshot
		if err := decoder.Decode(&snapshot); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if len(snapshots) == 0 {
		return nil, errors.New("no snapshot decoded")
	}
	return snapshots, nil
}

// bodyPrefix renders a short, safe excerpt of a rejected body for the error
// message. Samples carry user UUIDs and emails, so this stays short enough to
// identify the encoding without spilling a meaningful amount of user data.
func bodyPrefix(body []byte) string {
	const limit = 120
	excerpt := bytes.TrimSpace(body)
	if len(excerpt) > limit {
		return string(excerpt[:limit]) + "..."
	}
	return string(excerpt)
}

func (h *Handler) ping(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.Ping())
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	ok, message := h.service.Health()
	status := http.StatusOK
	if !ok {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":  map[bool]string{true: "ok", false: "degraded"}[ok],
		"message": message,
	})
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.db == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":   "not_ready",
			"database": "not_configured",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := h.db.PingContext(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":   "not_ready",
			"database": "unavailable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ready",
		"database": "ok",
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.Status())
}

func (h *Handler) collectAndRate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := h.service.RunCollectAndRate(r.Context(), "collect-and-rate")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) reconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := h.service.RunCollectAndRate(r.Context(), "reconcile")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

var _ = model.JobResult{}
