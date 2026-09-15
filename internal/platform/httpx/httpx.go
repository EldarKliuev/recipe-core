// Package httpx holds shared HTTP request/response plumbing: JSON encoding,
// a single error envelope shape, and mapping of domain errors to status codes.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
)

// Sentinel errors that service layers return and handlers translate to status
// codes. Keeping them here avoids handlers importing each other's packages.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrInvalidInput  = errors.New("invalid input")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrUnavailable   = errors.New("dependency unavailable")
	ErrTooManyTries  = errors.New("too many requests")
	ErrPayloadTooBig = errors.New("payload too large")
)

// Error is the single error envelope returned by every endpoint.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorBody struct {
	Error Error `json:"error"`
}

func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", slog.Any("error", err))
	}
}

func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Fail writes err as a JSON envelope, choosing the status from the wrapped
// sentinel. Unmapped errors become 500 and the detail is not leaked.
func Fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := classify(err)
	msg := err.Error()
	if status == http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request failed",
			slog.String("path", r.URL.Path),
			slog.Any("error", err))
		msg = "internal error"
	}
	JSON(w, status, errorBody{Error{Code: code, Message: msg}})
}

func classify(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "invalid_input"
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable, "unavailable"
	case errors.Is(err, ErrTooManyTries):
		return http.StatusTooManyRequests, "too_many_requests"
	case errors.Is(err, ErrPayloadTooBig):
		return http.StatusRequestEntityTooLarge, "payload_too_large"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

// Invalidf builds a 400-mapped error with a caller-supplied message.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

const maxBodyBytes = 1 << 20 // 1 MiB

// Decode reads a JSON body into dst, rejecting unknown fields so that typos in
// client payloads surface as errors instead of silently defaulting.
func Decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return Invalidf("malformed JSON body: %v", err)
	}
	return nil
}

// QueryInt reads an optional positive integer query parameter.
func QueryInt(r *http.Request, key string, def, min, max int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, Invalidf("%s must be an integer", key)
	}
	if v < min || v > max {
		return 0, Invalidf("%s must be between %d and %d", key, min, max)
	}
	return v, nil
}
