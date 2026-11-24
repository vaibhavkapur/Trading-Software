package integration

import (
	"encoding/json"
	"io"
	"net/http"
)

func jsonDecode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

func readAll(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
