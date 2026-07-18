package sidecar

// Agones REST (grpc-gateway equivalent) surface on localhost:9358.
// Routes and JSON shapes follow the Agones SDK REST
// mapping so tooling written against it keeps working:
//
//	POST /ready | /allocate | /shutdown | /health   body: {}
//	POST /reserve                                   body: {"seconds": "10"}
//	PUT  /metadata/label | /metadata/annotation     body: {"key","value"}
//	GET  /gameserver
//	GET  /watch/gameserver          newline-delimited {"result": GameServer}
import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"

	gatewayv1 "github.com/moepig/arena/gen/arena/gateway/v1"
)

// NewAgonesRESTHandler returns the REST adapter over the same Sidecar.
func NewAgonesRESTHandler(sc *Sidecar) http.Handler {
	h := &agonesREST{sc: sc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ready", h.simple(func() *gatewayv1.SidecarMessage {
		return &gatewayv1.SidecarMessage{Msg: &gatewayv1.SidecarMessage_Ready{Ready: &gatewayv1.ReadyRequest{}}}
	}))
	mux.HandleFunc("POST /allocate", h.simple(func() *gatewayv1.SidecarMessage {
		return &gatewayv1.SidecarMessage{Msg: &gatewayv1.SidecarMessage_Allocate{Allocate: &gatewayv1.SelfAllocateRequest{}}}
	}))
	mux.HandleFunc("POST /shutdown", h.simple(func() *gatewayv1.SidecarMessage {
		return &gatewayv1.SidecarMessage{Msg: &gatewayv1.SidecarMessage_Shutdown{Shutdown: &gatewayv1.ShutdownRequest{}}}
	}))
	mux.HandleFunc("POST /health", h.health)
	mux.HandleFunc("POST /reserve", h.reserve)
	mux.HandleFunc("PUT /metadata/label", h.metadata(gatewayv1.SetMetadataRequest_KIND_LABEL))
	mux.HandleFunc("PUT /metadata/annotation", h.metadata(gatewayv1.SetMetadataRequest_KIND_ANNOTATION))
	mux.HandleFunc("GET /gameserver", h.getGameServer)
	mux.HandleFunc("GET /watch/gameserver", h.watchGameServer)
	return mux
}

type agonesREST struct {
	sc *Sidecar
}

func (h *agonesREST) simple(build func() *gatewayv1.SidecarMessage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h.sc.send(r.Context(), build()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeEmpty(w)
	}
}

// health treats each POST as one health ping (the gRPC surface streams).
func (h *agonesREST) health(w http.ResponseWriter, _ *http.Request) {
	h.sc.recordHealth()
	writeEmpty(w)
}

func (h *agonesREST) reserve(w http.ResponseWriter, r *http.Request) {
	// grpc-gateway encodes int64 as a JSON string; accept a number too.
	var body struct {
		Seconds json.Number `json:"seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("bad reserve body: %v", err), http.StatusBadRequest)
		return
	}
	seconds, err := strconv.ParseInt(body.Seconds.String(), 10, 64)
	if body.Seconds.String() == "" {
		seconds, err = 0, nil
	}
	if err != nil || seconds < 0 {
		http.Error(w, "seconds must be a non-negative integer", http.StatusBadRequest)
		return
	}
	msg := &gatewayv1.SidecarMessage{
		Msg: &gatewayv1.SidecarMessage_Reserve{Reserve: &gatewayv1.ReserveRequest{Seconds: seconds}},
	}
	if err := h.sc.send(r.Context(), msg); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeEmpty(w)
}

func (h *agonesREST) metadata(kind gatewayv1.SetMetadataRequest_Kind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
			http.Error(w, "body must carry key (and value)", http.StatusBadRequest)
			return
		}
		msg := &gatewayv1.SidecarMessage{
			Msg: &gatewayv1.SidecarMessage_SetMetadata{SetMetadata: &gatewayv1.SetMetadataRequest{
				Kind: kind, Key: body.Key, Value: body.Value,
			}},
		}
		if err := h.sc.send(r.Context(), msg); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeEmpty(w)
	}
}

func (h *agonesREST) getGameServer(w http.ResponseWriter, _ *http.Request) {
	gs := h.sc.State()
	if gs == nil {
		http.Error(w, "gameserver state not received yet", http.StatusServiceUnavailable)
		return
	}
	b, err := protojson.Marshal(ToAgonesGameServer(gs))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// watchGameServer streams newline-delimited {"result": GameServer} objects,
// matching the grpc-gateway server-streaming encoding.
func (h *agonesREST) watchGameServer(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	id, ch := h.sc.watch()
	defer h.sc.unwatch(id)
	w.Header().Set("Content-Type", "application/json")
	// Commit headers before the first event so clients unblock immediately.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case gs := <-ch:
			b, err := protojson.Marshal(ToAgonesGameServer(gs))
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "{\"result\":%s}\n", b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEmpty(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}
