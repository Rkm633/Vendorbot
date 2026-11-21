package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"wa-server/central"
)

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func cleanUsername(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, "@")
	return strings.ToLower(u)
}

// RequireCentralAuth protects inbound Central Brain Node -> Vendor Bot webhook endpoints using VENDOR_API_KEY.
func RequireCentralAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !central.Client.VerifyWebhookAuth(r) {
			respondJSON(w, http.StatusUnauthorized, map[string]string{
				"status":  "error",
				"message": "Unauthorized: invalid or missing VENDOR_API_KEY",
			})
			return
		}
		next(w, r)
	}
}
