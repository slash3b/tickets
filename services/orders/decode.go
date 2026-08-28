package orders

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/slash3b/tickets/pkg/rpc"
)

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "malformed body")
		return false
	}
	return true
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "malformed "+name)
		return uuid.Nil, false
	}
	return id, true
}
