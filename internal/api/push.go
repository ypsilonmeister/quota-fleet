package api

import (
	"errors"
	"net/http"

	"github.com/ypsilonmeister/quota-fleet/internal/push"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// handleVAPIDPublicKey returns the key browsers subscribe with (docs/16).
func (s *Server) handleVAPIDPublicKey(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, VAPIDPublicKeyResponse{PublicKey: s.webPushPublicKey})
}

// handleCreatePushSubscription registers a browser, or updates its keys.
func (s *Server) handleCreatePushSubscription(w http.ResponseWriter, r *http.Request) {
	var req PushSubscriptionRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sub, err := push.NewSubscription(req.Endpoint, req.Keys.P256dh, req.Keys.Auth, r.UserAgent())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The subscription receives only while this token may read jobs (docs/16).
	principal, _ := authPrincipalFromContext(r.Context())
	sub.TokenID = principal.tokenID
	created, err := s.store.UpsertPushSubscription(r.Context(), sub)
	if err != nil {
		if errors.Is(err, store.ErrPushEndpointDeleted) {
			writeError(w, http.StatusGone, err.Error())
			return
		}
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, PushSubscriptionResponse{Created: created})
}

// handleDeletePushSubscription removes the subscription for an endpoint. The endpoint is
// taken from the body because it is a capability URL that must not end up in access logs.
func (s *Server) handleDeletePushSubscription(w http.ResponseWriter, r *http.Request) {
	var req DeletePushSubscriptionRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Endpoint == "" {
		writeError(w, http.StatusBadRequest, "endpoint is required")
		return
	}
	deleted, err := s.store.DeletePushSubscription(r.Context(), req.Endpoint)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, DeletePushSubscriptionResponse{Deleted: deleted})
}
