package db

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/larsakerlund/loft/internal/web"
)

var (
	errNotFound       = errors.New("not found")
	errForbidden      = errors.New("owner-only: not your document")
	errQuota          = errors.New("document quota reached")
	errPolicyConflict = errors.New("collection ownerOnly policy already set to a different value")
)

// merge reconstructs the API document: the stored fields plus the authoritative id and creator.
func merge(docJSON, id string, creator *string) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal([]byte(docJSON), &m)
	m["id"] = id
	if creator != nil {
		m["creator"] = *creator
	} else {
		m["creator"] = nil
	}
	return m
}

// readJSONObject reads a size-capped JSON object body, writing the appropriate error and returning
// false on overflow (413), parse failure or a non-object (400).
func readJSONObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDocBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		web.Error(w, http.StatusRequestEntityTooLarge, "document too large")
		return nil, false
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		web.Error(w, http.StatusBadRequest, "body must be a JSON object")
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		web.Error(w, http.StatusBadRequest, "body must be a JSON object")
		return nil, false
	}
	return m, true
}

func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNotFound):
		web.Error(w, http.StatusNotFound, "not found")
	case errors.Is(err, errForbidden):
		web.Error(w, http.StatusForbidden, errForbidden.Error())
	case errors.Is(err, errQuota):
		web.Error(w, http.StatusConflict, "document quota reached")
	case errors.Is(err, errPolicyConflict):
		web.Error(w, http.StatusConflict, errPolicyConflict.Error())
	case errors.Is(err, context.Canceled):
		// Client went away mid-request; nothing to write and nothing alarming to log.
	case errors.Is(err, context.DeadlineExceeded):
		// The per-transaction deadline fired, usually pool saturation. Signal back-pressure, not 500.
		log.Printf("loftd db timeout (saturation?): %v", err)
		web.Error(w, http.StatusServiceUnavailable, "service busy, try again")
	default:
		log.Printf("loftd db error: %v", err)
		web.Error(w, http.StatusInternalServerError, "db error")
	}
}
