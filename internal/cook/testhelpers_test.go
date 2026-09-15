package cook

import (
	"encoding/json"
	"net/http"
)

func decodeJSON(r *http.Request, dst any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(dst)
}
