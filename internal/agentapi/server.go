// SPDX-License-Identifier: AGPL-3.0-or-later

// Package agentapi exposes the Agent Branch API: a small HTTP service that
// gives each AI agent its own instant, isolated database branch.
//
//	POST   /agents/{id}/branch   -> create a branch for the agent, return a DSN
//	DELETE /agents/{id}/branch   -> tear the agent's branch down
//	GET    /agents               -> list active agent branches
//	GET    /healthz              -> liveness
package agentapi

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/vectoradb/vectoradb/internal/branch"
)

// Serve starts the Agent Branch API on addr (e.g. ":8088").
func Serve(addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		infos, err := branch.ListAgentBranches()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if infos == nil {
			infos = []branch.Info{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": infos})
	})

	mux.HandleFunc("POST /agents/{id}/branch", func(w http.ResponseWriter, r *http.Request) {
		info, err := branch.CreateAgentBranch(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusCreated, info)
	})

	mux.HandleFunc("DELETE /agents/{id}/branch", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := branch.DeleteAgentBranch(id); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"agent": id, "status": "deleted"})
	})

	log.Printf("agent branch API listening on %s", addr)
	return http.ListenAndServe(addr, cors(logging(mux)))
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
