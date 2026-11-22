// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package api is the HTTP interface. Every endpoint except /healthz needs HTTP Basic credentials, because the
// API can read and write every database it has settings for. Errors are RFC 9457 problem details.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/ergulcu/data-transporter/internal/configs"
	"github.com/ergulcu/data-transporter/internal/jobs"
	"github.com/ergulcu/data-transporter/internal/transfer"
)

// FieldError is one invalid field of a request.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Problem is an RFC 9457 problem detail.
type Problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	Errors   []FieldError `json:"errors,omitempty"`
}

// Credentials of the API user.
type Credentials struct {
	User, Password string
}

type server struct {
	configs     *configs.Store
	transporter *transfer.Transporter
	jobs        *jobs.Jobs
	log         *slog.Logger
	user, pass  [32]byte
}

// New returns the handler.
func New(c *configs.Store, t *transfer.Transporter, j *jobs.Jobs, creds Credentials, log *slog.Logger) http.Handler {
	s := &server{configs: c, transporter: t, jobs: j, log: log,
		user: sha256.Sum256([]byte(creds.User)), pass: sha256.Sum256([]byte(creds.Password))}
	api := http.NewServeMux()
	api.HandleFunc("POST /db/config", s.saveConfig)
	api.HandleFunc("GET /db/config/{code}", s.getConfig)
	api.HandleFunc("GET /db/configs", s.listConfigs)
	api.HandleFunc("DELETE /db/config/{code}", s.deleteConfig)
	api.HandleFunc("POST /db/config/{code}/test", s.testConfig)
	api.HandleFunc("POST /db/migrate", s.migrate)
	api.HandleFunc("POST /db/migrations", s.startMigration)
	api.HandleFunc("GET /db/migrations", s.listMigrations)
	api.HandleFunc("GET /db/migrations/{id}", s.getMigration)
	api.HandleFunc("DELETE /db/migrations/{id}", s.cancelMigration)
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, "", nil)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := c.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "DOWN"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "UP"})
	})
	mux.Handle("/", s.authenticated(api))
	return mux
}

// authenticated checks HTTP Basic credentials in constant time.
func (s *server) authenticated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		u, p := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(pass))
		if !ok || subtle.ConstantTimeCompare(u[:], s.user[:])&subtle.ConstantTimeCompare(p[:], s.pass[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="datatransporter", charset="UTF-8"`)
			writeProblem(w, r, http.StatusUnauthorized, "", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// saveConfig creates (201) or replaces (200) the settings with the given code.
func (s *server) saveConfig(w http.ResponseWriter, r *http.Request) {
	var c configs.Config
	if !decode(w, r, &c) {
		return
	}
	created, err := s.configs.Save(r.Context(), c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	view, err := s.configs.Find(r.Context(), c.Code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, view)
		return
	}
	w.Header().Set("Location", "/db/config/"+url.PathEscape(c.Code))
	writeJSON(w, http.StatusCreated, view)
}

func (s *server) getConfig(w http.ResponseWriter, r *http.Request) {
	view, err := s.configs.Find(r.Context(), r.PathValue("code"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// listConfigs answers an object keyed by code, as the Java version did.
func (s *server) listConfigs(w http.ResponseWriter, r *http.Request) {
	views, err := s.configs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byCode := make(map[string]configs.View, len(views))
	for _, v := range views {
		byCode[v.Code] = v
	}
	writeJSON(w, http.StatusOK, byCode)
}

func (s *server) deleteConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.configs.Delete(r.Context(), r.PathValue("code")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) testConfig(w http.ResponseWriter, r *http.Request) {
	check, err := s.transporter.CheckConnection(r.Context(), r.PathValue("code"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, check)
}

// migrate runs the migration while the request waits: 200 when it succeeded, 422 when it failed.
func (s *server) migrate(w http.ResponseWriter, r *http.Request) {
	req, ok := s.migrationRequest(w, r)
	if !ok {
		return
	}
	res, err := s.transporter.Migrate(r.Context(), req, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if !res.Success {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, res)
}

// startMigration starts the migration in the background; follow it at the returned location.
func (s *server) startMigration(w http.ResponseWriter, r *http.Request) {
	req, ok := s.migrationRequest(w, r)
	if !ok {
		return
	}
	// Unknown codes are reported now rather than in the background.
	for _, code := range []string{req.SourceCode, req.TargetCode} {
		if _, err := s.configs.Find(r.Context(), code); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	job := s.jobs.Start(req)
	w.Header().Set("Location", "/db/migrations/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (s *server) listMigrations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.jobs.List())
}

func (s *server) getMigration(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Find(r.PathValue("id"))
	if !ok {
		writeProblem(w, r, http.StatusNotFound, "No migration with id '"+r.PathValue("id")+"'", nil)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *server) cancelMigration(w http.ResponseWriter, r *http.Request) {
	if !s.jobs.Cancel(r.PathValue("id")) {
		writeProblem(w, r, http.StatusNotFound, "No migration with id '"+r.PathValue("id")+"'", nil)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *server) migrationRequest(w http.ResponseWriter, r *http.Request) (transfer.Request, bool) {
	var req transfer.Request
	if !decode(w, r, &req) {
		return req, false
	}
	if errs := req.Validate(); len(errs) > 0 {
		writeProblem(w, r, http.StatusBadRequest, "Invalid request content.", fieldErrors(errs))
		return req, false
	}
	return req, true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Failed to read request: "+err.Error(), nil)
		return false
	}
	return true
}

func fieldErrors(errs map[string]string) []FieldError {
	out := make([]FieldError, 0, len(errs))
	for _, f := range slices.Sorted(maps.Keys(errs)) {
		out = append(out, FieldError{Field: f, Message: errs[f]})
	}
	return out
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var notFound *configs.NotFoundError
	var invalid *configs.InvalidError
	switch {
	case errors.As(err, &notFound):
		writeProblem(w, r, http.StatusNotFound, notFound.Error(), nil)
	case errors.As(err, &invalid):
		writeProblem(w, r, http.StatusBadRequest, "Invalid request content.", fieldErrors(invalid.Fields))
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeProblem(w, r, http.StatusInternalServerError, err.Error(), nil)
	}
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string, errs []FieldError) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Problem{Type: "about:blank", Title: http.StatusText(status), Status: status, //nolint:errcheck
		Detail: detail, Instance: r.URL.Path, Errors: errs})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
