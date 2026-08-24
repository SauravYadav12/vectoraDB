// SPDX-License-Identifier: AGPL-3.0-or-later

// Package controlplane serves VectoraDB's management REST API and web dashboard.
package controlplane

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/daemon"
)

//go:embed dashboard.html
var dashboardHTML []byte

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Serve starts the control plane + dashboard on addr (e.g. ":8080").
func Serve(addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})

	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		bs, err := branch.Branches()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		var nBranch, nAgent int
		mainReady := false
		for _, b := range bs {
			switch {
			case b.Primary:
				mainReady = b.State == "running"
			case b.Agent:
				nAgent++
			default:
				nBranch++
			}
		}
		writeJSON(w, 200, map[string]any{
			"mainReady": mainReady,
			"branches":  nBranch,
			"agents":    nAgent,
			"ha":        branch.HAInfo(),
			"servers": map[string]bool{
				"proxy": daemon.Alive("proxy"),
				"api":   daemon.Alive("api"),
			},
		})
	})

	mux.HandleFunc("GET /api/branches", func(w http.ResponseWriter, r *http.Request) {
		bs, err := branch.Branches()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		if bs == nil {
			bs = []branch.BranchInfo{}
		}
		writeJSON(w, 200, bs)
	})

	mux.HandleFunc("POST /api/branches", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !nameRe.MatchString(body.Name) {
			writeErr(w, 400, fmt.Errorf("invalid name (use lowercase letters, digits, dashes)"))
			return
		}
		if err := branch.Create(body.Name, ""); err != nil {
			writeErr(w, 409, err)
			return
		}
		writeJSON(w, 201, map[string]string{"name": body.Name, "status": "created"})
	})

	mux.HandleFunc("DELETE /api/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := branch.Delete(r.PathValue("name")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
	})

	mux.HandleFunc("POST /api/branches/{name}/suspend", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name == "main" {
			writeErr(w, 400, fmt.Errorf("refusing to suspend the primary 'main'"))
			return
		}
		if err := branch.Suspend(name); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "suspended"})
	})

	mux.HandleFunc("POST /api/branches/{name}/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := branch.Wake(r.PathValue("name")); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "resumed"})
	})

	log.Printf("control plane + dashboard on %s", addr)
	return http.ListenAndServe(addr, logging(mux))
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
		if r.URL.Path != "/api/status" && r.URL.Path != "/api/branches" {
			log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
