package httpmock

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

var nsRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Handler exposes the data plane and the control plane of the hub.
//
//	GET    /healthz
//	ANY    /ns/{ns}/{mock}/{path...}                      data plane
//	PUT    /_mock/ns/{ns}/mocks/{mock}                     configure (MockConfig)
//	PUT    /_mock/ns/{ns}/mocks/{mock}/script              script (Script)
//	GET    /_mock/ns/{ns}/mocks                            list mocks + provenance
//	GET    /_mock/ns/{ns}/journal[?mock=name]              journal
//	POST   /_mock/ns/{ns}/webhooks                         send webhook to the SUT
//	DELETE /_mock/ns/{ns}                                  reset namespace
//	GET    /_mock/namespaces                               list namespaces
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/ns/", h.dataPlane)
	mux.HandleFunc("PUT /_mock/ns/{ns}/mocks/{mock}", h.ctl(func(w http.ResponseWriter, r *http.Request, ns string) {
		var cfg MockConfig
		if !decode(w, r, &cfg) {
			return
		}
		if err := h.Configure(ns, r.PathValue("mock"), cfg); err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "configured"})
	}))
	mux.HandleFunc("PUT /_mock/ns/{ns}/mocks/{mock}/script", h.ctl(func(w http.ResponseWriter, r *http.Request, ns string) {
		var s Script
		if !decode(w, r, &s) {
			return
		}
		if err := h.SetScript(ns, r.PathValue("mock"), s); err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, map[string]string{"status": "scripted"})
	}))
	mux.HandleFunc("GET /_mock/ns/{ns}/mocks", h.ctl(func(w http.ResponseWriter, _ *http.Request, ns string) {
		writeJSON(w, h.Mocks(ns))
	}))
	mux.HandleFunc("GET /_mock/ns/{ns}/journal", h.ctl(func(w http.ResponseWriter, r *http.Request, ns string) {
		writeJSON(w, h.Journal(ns, r.URL.Query().Get("mock")))
	}))
	mux.HandleFunc("POST /_mock/ns/{ns}/webhooks", h.ctl(func(w http.ResponseWriter, r *http.Request, ns string) {
		var req WebhookRequest
		if !decode(w, r, &req) {
			return
		}
		res, err := h.SendWebhook(r.Context(), ns, req)
		if err != nil {
			httpErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, res)
	}))
	mux.HandleFunc("DELETE /_mock/ns/{ns}", h.ctl(func(w http.ResponseWriter, _ *http.Request, ns string) {
		h.Reset(ns)
		writeJSON(w, map[string]string{"status": "reset"})
	}))
	mux.HandleFunc("GET /_mock/namespaces", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, h.Namespaces())
	})
	return mux
}

func (h *Hub) dataPlane(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/ns/"), "/", 3)
	if len(parts) < 2 || !nsRe.MatchString(parts[0]) || parts[1] == "" {
		httpErr(w, http.StatusNotFound, "mockhub: expected /ns/{ns}/{mock}/{path}")
		return
	}
	rest := "/"
	if len(parts) == 3 {
		rest += parts[2]
	}
	h.ServeData(w, r, parts[0], parts[1], rest)
}

func (h *Hub) ctl(fn func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ns := r.PathValue("ns")
		if !nsRe.MatchString(ns) {
			httpErr(w, http.StatusBadRequest, "invalid namespace")
			return
		}
		fn(w, r, ns)
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		httpErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
