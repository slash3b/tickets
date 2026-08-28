package catalog

import (
	"encoding/json"
	"net/http"

	"github.com/slash3b/tickets/pkg/rpc"
)

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		rpc.Fail(w, http.StatusBadRequest, "bad_request", "malformed body")
		return false
	}
	return true
}
