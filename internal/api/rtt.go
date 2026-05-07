package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/go-chi/chi/v5"
)

func (s *Server) doSendLegRTT(ctx context.Context, id, text string) error {
	l, ok := s.LegMgr.Get(id)
	if !ok {
		return newAPIError(http.StatusNotFound, "leg not found")
	}
	if text == "" {
		return newAPIError(http.StatusBadRequest, "text required")
	}
	if !l.RTTNegotiated() {
		return newAPIError(http.StatusConflict, "RTT not negotiated for this leg")
	}
	if err := l.SendText(ctx, text); err != nil {
		if errors.Is(err, leg.ErrRTTNotNegotiated) {
			return newAPIError(http.StatusConflict, "%s", err.Error())
		}
		return newAPIError(http.StatusInternalServerError, "%s", err.Error())
	}
	return nil
}

func (s *Server) sendRTT(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req RTTRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.doSendLegRTT(r.Context(), id, req.Text); err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *Server) acceptRTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	l.SetAcceptText(true)
	writeJSON(w, http.StatusOK, map[string]string{"status": "rtt_accepting"})
}

func (s *Server) rejectRTTLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	l.SetAcceptText(false)
	writeJSON(w, http.StatusOK, map[string]string{"status": "rtt_rejecting"})
}

// broadcastRTT forwards a chunk of received text to every other RTT-capable
// leg in the same room that has accept_text enabled. Mirrors broadcastDTMF.
func (s *Server) broadcastRTT(fromLegID, text string) {
	roomID, ok := s.RoomMgr.FindLegRoom(fromLegID)
	if !ok {
		return
	}
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok {
		return
	}
	for _, p := range rm.Participants() {
		if p.ID() == fromLegID || !p.AcceptText() || !p.RTTNegotiated() {
			continue
		}
		go func(target leg.Leg) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := target.SendText(ctx, text); err != nil {
				s.Log.Warn("rtt forward failed", "from_leg", fromLegID, "to_leg", target.ID(), "error", err)
			}
		}(p)
	}
}
