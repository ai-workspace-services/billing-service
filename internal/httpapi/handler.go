package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"billing-service/internal/model"
	"billing-service/internal/service"
)

type Handler struct {
	service *service.Service
	token   string
}

func New(svc *service.Service, token string) *Handler {
	return &Handler{service: svc, token: strings.TrimSpace(token)}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ping", h.ping)
	mux.HandleFunc("/healthz", h.healthz)
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
	var snapshot model.Snapshot
	if err := json.NewDecoder(r.Body).Decode(&snapshot); err != nil {
		http.Error(w, "invalid snapshot: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := h.service.IngestSnapshot(r.Context(), snapshot)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, result)
		return
	}
	writeJSON(w, http.StatusOK, result)
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
