// Package reply is the one JSON shape every HTTP response in this project
// uses, for the app-facing API and for the direct ABS/Monnify test endpoints
// alike:
//
//	{"status": "successful", "message": "...", "data": {...}}
//	{"status": "failed", "error": "..."}
//
// "status" describes whether this HTTP call itself could be carried out, not
// the business outcome inside it: a spend call that reached ABS and got back
// a definite decline is still a "successful" call whose data.state is
// "FAILED". "status" is "failed" only when the call itself could not be
// completed — bad input, an unreachable provider, an internal error.
package reply

import (
	"encoding/json"
	"net/http"
)

type Envelope struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// OK writes a successful envelope carrying data, at the given HTTP status
// (normally http.StatusOK).
func OK(w http.ResponseWriter, httpStatus int, data any) {
	write(w, httpStatus, Envelope{Status: "successful", Data: data})
}

// OKWithMessage is OK with a human-readable message alongside the data.
func OKWithMessage(w http.ResponseWriter, httpStatus int, message string, data any) {
	write(w, httpStatus, Envelope{Status: "successful", Message: message, Data: data})
}

// Fail writes a failed envelope: the call could not be completed. message is
// shown to the caller, so it must never contain a secret or an internal detail
// the client should not see (log those separately and pass a plain message here).
func Fail(w http.ResponseWriter, httpStatus int, message string) {
	write(w, httpStatus, Envelope{Status: "failed", Error: message})
}

func write(w http.ResponseWriter, httpStatus int, e Envelope) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(e)
}
