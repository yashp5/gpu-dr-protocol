// Package writeresp provides functions to write JSON responses in a consistent format.
// Use JSONData to write a successful response with data, and JSONErr to write an error response.
// Format of the JSON response.
//
// # Example: JSON Data Response
//
//	{
//	  "data": {
//	    "foo": "bar"
//	  },
//	  "metadata": {
//	    "status_code": 200,
//	    "span_id": "0196ca51-2218-7400-92cd-045292290275",
//	    "trace_id": "0196ca51-2218-7405-ae27-6b70a918c8fe"
//	  }
//	}
//
// # Example: JSON Error Response
//
//	{
//	  "error": "foo",
//	  "metadata": {
//	    "status_code": 500,
//	    "span_id": "0196ca51-2218-7400-92cd-045292290275",
//		"trace_id": "0196ca51-2218-7405-ae27-6b70a918c8fe"
//	  }
//	}
package writeresp

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/nervus-labs/nervus-protocol/trace"
)

// Response is the structure of the JSON response sent to the client.
// See the package documentation for the format of the response.
type Response struct {
	Data     any      `json:"data,omitempty,omitzero"`  // Exactly one of Data or Error should be set.
	Error    string   `json:"error,omitempty,omitzero"` // Exactly one of Data or Error should be set.
	Metadata Metadata `json:"metadata,omitempty,omitzero"`
}

// JSONErr writes a JSON response with the given status code in the format
//
//	{
//		"error": err.Error(),
//		"metadata": {"status_code": statusCode, "trace_id": trace.TraceID, "request_id": trace.RequestID
//	}
//
// It also sets the Content-Type header to "application/json" and writes the status code to the response.
// Where the traceID & requestID are taken from the context if available.
// If no statusCode is provided, it defaults to 500 Internal Server Error.
func JSONErr(ctx context.Context, w http.ResponseWriter, err error, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	if err == nil {
		panic("nil error provided to WriteErrorResponse") // programmer error. the panic handler middleware will surface this to us and give the user a 500 error
	}
	switch {
	case statusCode < 0:
		panic("negative status code provided to WriteErrorResponse") // programmer error. the panic handler middleware will surface this to us and give the user a 500 error
	case statusCode == 0:
		statusCode = http.StatusInternalServerError // default to 500 if no status code is provided
	}
	meta := Metadata{
		StatusCode: statusCode,
	}
	tr, ok := trace.FromCtx(ctx)
	if ok {
		meta.RequestID = tr.RequestID
		meta.TraceID = tr.TraceID
	}

	if statusCode != http.StatusOK {
		w.WriteHeader(statusCode)
	}

	json.NewEncoder(w).Encode(&Response{
		Error:    err.Error(),
		Metadata: meta,
	})
}

// JSONData writes a JSON response with the given status code in the format
//
//	{
//	  "data": data,
//	  "metadata": {"status_code": statusCode, "trace_id": trace.TraceID, "request_id": trace.RequestID}
//	}
//
// It also sets the Content-Type header to "application/json" and writes the status code to the response.
// Where the traceID & requestID are taken from the context if available.
// if no statusCode is provided, it defaults to 200 OK.
// Do not provide an error value to this function: use JSONErr instead.
func JSONData(ctx context.Context, w http.ResponseWriter, data any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case statusCode < 0:
		panic("negative status code provided to WriteErrorResponse") // programmer error. the panic handler middleware will surface this to us and give the user a 500 error
	case statusCode == 0:
		statusCode = http.StatusOK // default to 200 OK if no status code is provided
	}

	switch data.(type) {
	case nil:
		panic("nil value provided to WriteResponse") // programmer error. the panic handler middleware will surface this to us and give the user a 500 error
	case error:
		panic("error value provided to writeDataResponse: did you mean to use writeErrorResponse?") // programmer error. the panic handler middleware will surface this to us and give the user a 500 error
	default:
		// nop
	}

	meta := Metadata{
		StatusCode: statusCode,
	}
	tr, ok := trace.FromCtx(ctx)
	if ok {
		meta.RequestID = tr.RequestID
		meta.TraceID = tr.TraceID
	}

	body := Response{Data: data, Metadata: meta}
	w.Header().Set("Content-Type", "application/json")

	if statusCode != http.StatusOK {
		w.WriteHeader(statusCode)
	}
	json.NewEncoder(w).Encode(&body)
}

// Response Metadata. Contains the status code, trace ID, and request ID.
// See the trace package for more information on trace IDs and request IDs.
type Metadata struct {
	StatusCode int    `json:"status_code,omitzero"`
	TraceID    string `json:"trace_id,omitzero"`
	RequestID  string `json:"request_id,omitzero"`
}
