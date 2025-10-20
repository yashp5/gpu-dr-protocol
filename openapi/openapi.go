package openapi

import (
	_ "embed"
	"fmt"
	"net/http"
)

//go:embed openapi.json
var b []byte

// Handler serves the OpenAPI JSON specification.
// We do NOT serve a UI - it's bloated and huge. If our customers want to do that, we can point them at any number of implementations - since we serve the JSON file,
// they can use it from there.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(b); err != nil {
		http.Error(w, fmt.Sprintf("failed to write OpenAPI JSON: %v", err), http.StatusInternalServerError)
		return

	}
}
