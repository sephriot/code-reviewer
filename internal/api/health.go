// Package api exposes the reviewd control HTTP API.
package api

import (
	"context"
	"encoding/json"
	"net/http"
)

const jsonContentType = "application/json"

// SchemaStatus describes the control-plane migration state used by readiness.
type SchemaStatus struct {
	Current int `json:"current"`
	Latest  int `json:"latest"`
	Pending int `json:"pending"`
}

// Readiness contains the database checks needed to admit control API traffic.
// Callbacks keep the transport independent of a particular persistence adapter.
type Readiness struct {
	Ping         func(context.Context) error
	SchemaStatus func(context.Context) (SchemaStatus, error)
}

type healthChecks struct {
	Database string `json:"database"`
	Schema   string `json:"schema"`
}

type healthResponse struct {
	Status       string        `json:"status"`
	Checks       *healthChecks `json:"checks,omitempty"`
	SchemaStatus *SchemaStatus `json:"schema_status,omitempty"`
}

// NewHealthHandler returns the liveness and readiness routes.
func NewHealthHandler(readiness Readiness) http.Handler {
	mux := http.NewServeMux()
	registerHealthRoutes(mux, readiness)
	return mux
}

func registerHealthRoutes(mux *http.ServeMux, readiness Readiness) {
	mux.HandleFunc("GET /api/v1/health/live", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, healthResponse{Status: "live"})
	})
	mux.HandleFunc("GET /api/v1/health/ready", func(response http.ResponseWriter, request *http.Request) {
		handleReady(response, request, readiness)
	})
}

func handleReady(response http.ResponseWriter, request *http.Request, readiness Readiness) {
	body := healthResponse{
		Status: "not_ready",
		Checks: &healthChecks{Database: "unknown", Schema: "unknown"},
	}
	if readiness.Ping == nil || readiness.SchemaStatus == nil {
		writeJSON(response, http.StatusServiceUnavailable, body)
		return
	}
	if err := readiness.Ping(request.Context()); err != nil {
		body.Checks.Database = "failed"
		writeJSON(response, http.StatusServiceUnavailable, body)
		return
	}
	body.Checks.Database = "ok"

	status, err := readiness.SchemaStatus(request.Context())
	body.SchemaStatus = &status
	if err != nil || !validSchemaStatus(status) {
		body.Checks.Schema = "failed"
		writeJSON(response, http.StatusServiceUnavailable, body)
		return
	}
	if status.Pending != 0 || status.Current != status.Latest {
		body.Checks.Schema = "pending"
		writeJSON(response, http.StatusServiceUnavailable, body)
		return
	}

	body.Status = "ready"
	body.Checks.Schema = "ok"
	writeJSON(response, http.StatusOK, body)
}

func validSchemaStatus(status SchemaStatus) bool {
	return status.Current >= 0 &&
		status.Latest >= 0 &&
		status.Pending >= 0 &&
		status.Current <= status.Latest &&
		status.Pending == status.Latest-status.Current
}

func writeJSON(response http.ResponseWriter, status int, body healthResponse) {
	response.Header().Set("Content-Type", jsonContentType)
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}
