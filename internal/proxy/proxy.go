// SPDX-License-Identifier: AGPL-3.0-or-later

// Package proxy is the vectoradb serverless front door: a single PostgreSQL
// wire-protocol endpoint that routes each connection to the right branch based
// on the database name in the client's startup message, then pipes the rest of
// the session through transparently.
package proxy

import (
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/vectoradb/vectoradb/internal/auth"
	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/tlsutil"
)

// authStore verifies API keys presented as the connection password. When nil
// (VECTORADB_GATEWAY_NOAUTH), the Gateway accepts any client and still mediates
// the backend login — convenient for trusted/local use.
var authStore *auth.Store

// tlsConfig, when non-nil, lets the Gateway answer SSLRequest with 'S' and wrap
// the client connection in TLS — so clients with sslmode=require connect and the
// API key never crosses the wire in cleartext. Loaded once in Serve.
var tlsConfig *tls.Config

// gatewayNoAuth reports whether the gateway's API-key check is disabled. The
// VECTORADB_GATEWAY_NOAUTH escape hatch is only honored in builds made with
// `-tags insecure`; release builds compile it out (insecureAllowed == false), so
// a full auth bypass can never be flipped on in production by an env var.
func gatewayNoAuth() bool {
	if !insecureAllowed {
		return false
	}
	v := os.Getenv("VECTORADB_GATEWAY_NOAUTH")
	return v == "1" || v == "true"
}

// lastActivity tracks when the proxy last routed a connection to each branch, so
// the reaper can suspend branches that have been idle.
var (
	mu           sync.Mutex
	lastActivity = map[string]time.Time{}
)

func touch(name string) {
	mu.Lock()
	lastActivity[name] = time.Now()
	mu.Unlock()
}

// realDatabase is the actual Postgres database inside every branch. The client's
// requested "database" is the branch NAME (routing key), which we rewrite to
// this before forwarding to the backend.
const realDatabase = "vectoradb"

// realUser is the Postgres role the Gateway logs clients in as — a non-superuser
// role, so client sessions obey RLS/GRANTs and cannot bypass the append-only
// ledger. The API key gates the client; this role bounds what they can do.
const realUser = "vdbclient"

const (
	codeStartup30 = 196608   // protocol 3.0 StartupMessage
	codeSSL       = 80877103 // SSLRequest
	codeGSS       = 80877104 // GSSENCRequest
)

// Serve listens on addr (e.g. ":6432") and proxies Postgres connections,
// auto-resuming suspended branches on connect and auto-suspending branches idle
// for longer than idle (0 disables suspension).
func Serve(addr string, idle time.Duration) error {
	if gatewayNoAuth() {
		log.Printf("gateway authentication DISABLED (VECTORADB_GATEWAY_NOAUTH) — trusted/local mode")
	} else {
		store, err := auth.OpenFromEnv()
		if err != nil {
			return fmt.Errorf("open auth store: %w", err)
		}
		authStore = store
		log.Printf("gateway authentication ENABLED — connect with an API key (vdb_…) as the password")
	}
	if cfg, err := tlsutil.ServerConfig(); err != nil {
		log.Printf("TLS disabled (could not load certificate): %v — clients must use sslmode=disable", err)
	} else {
		tlsConfig = cfg
		log.Printf("TLS enabled — clients can connect with sslmode=require")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if idle > 0 {
		go reaper(idle)
		log.Printf("auto-suspend enabled: idle branches stop after %s", idle)
	}
	log.Printf("wire-protocol proxy listening on %s — connect with dbname=<branch> (e.g. dbname=main)", addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go handle(c)
	}
}

func handle(client net.Conn) {
	defer client.Close()

	// readStartup may upgrade the connection to TLS, so it returns the conn to
	// use for the rest of the session (Go passes net.Conn by value).
	client, params, err := readStartup(client)
	if err != nil {
		log.Printf("startup: %v", err)
		return
	}

	// Gateway authentication: the client's password must be a valid API key.
	// The authenticated identity becomes the ledger "actor" for this session.
	var actor string
	if authStore != nil {
		key, err := requestClientPassword(client)
		if err != nil {
			log.Printf("gateway auth: %v", err)
			return
		}
		u, ok := authStore.VerifyKey(key)
		if !ok {
			sendError(client, "28P01", "invalid API key — use a vdb_ key as the password")
			return
		}
		actor = u.Email
	}

	target := params["database"]
	if target == "" {
		target = "main"
	}
	touch(target)
	if st := branch.ContainerState(target); st != "running" && st != "absent" {
		log.Printf("auto-resume: waking %s (was %s)", target, st)
	}
	addr, err := branch.EnsureRunning(target) // resumes the branch if suspended
	if err != nil {
		log.Printf("route dbname=%q: %v", target, err)
		sendError(client, "3D000", err.Error()) // invalid_catalog_name
		return
	}
	backend, err := net.Dial("tcp", addr)
	if err != nil {
		log.Printf("dial backend %s: %v", addr, err)
		sendError(client, "08006", fmt.Sprintf("could not reach branch %q", target))
		return
	}
	defer backend.Close()

	// Log in to the backend as the real role/database; the branch name and client
	// user were only routing/identity inputs. The Gateway performs the backend
	// handshake so the client never needs the real DB password.
	params["database"] = realDatabase
	params["user"] = realUser
	// Attribution for the schema ledger: inject connection context that the
	// branch's DDL event triggers read via current_setting('vectoradb.*').
	params["options"] = ledgerOptions(params["options"], actor, target)
	if err := backendAuth(backend, params); err != nil {
		log.Printf("backend auth %s: %v", addr, err)
		sendError(client, "08006", fmt.Sprintf("branch %q authentication failed", target))
		return
	}
	// Authentication (client + backend) complete — tell the client it's in.
	if err := writeAuthOk(client); err != nil {
		return
	}
	log.Printf("routed: dbname=%s -> %s", target, addr)

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, backend); done <- struct{}{} }()
	<-done
}

// readStartup reads the startup phase and returns the startup parameters (user,
// database, ...). When TLS is configured it accepts an SSLRequest, upgrades the
// connection, and returns the wrapped conn — the caller must use the returned
// conn for the rest of the session. GSS is always declined.
func readStartup(client net.Conn) (net.Conn, map[string]string, error) {
	for {
		header := make([]byte, 4)
		if _, err := io.ReadFull(client, header); err != nil {
			return client, nil, err
		}
		msgLen := int(binary.BigEndian.Uint32(header))
		if msgLen < 8 || msgLen > 1<<20 {
			return client, nil, fmt.Errorf("bad startup length %d", msgLen)
		}
		body := make([]byte, msgLen-4)
		if _, err := io.ReadFull(client, body); err != nil {
			return client, nil, err
		}
		switch binary.BigEndian.Uint32(body[:4]) {
		case codeSSL:
			if tlsConfig == nil {
				if _, err := client.Write([]byte{'N'}); err != nil { // we don't offer it
					return client, nil, err
				}
				continue
			}
			// Offer TLS: reply 'S', then wrap and hand the loop the encrypted
			// conn to read the real StartupMessage over.
			if _, err := client.Write([]byte{'S'}); err != nil {
				return client, nil, err
			}
			tconn := tls.Server(client, tlsConfig)
			if err := tconn.Handshake(); err != nil {
				return client, nil, fmt.Errorf("tls handshake: %w", err)
			}
			client = tconn
			continue
		case codeGSS:
			if _, err := client.Write([]byte{'N'}); err != nil { // GSS never offered
				return client, nil, err
			}
			continue
		case codeStartup30:
			return client, parseParams(body[4:]), nil
		default:
			return client, nil, fmt.Errorf("unsupported startup code %d", binary.BigEndian.Uint32(body[:4]))
		}
	}
}

// sendError writes a Postgres ErrorResponse (a FATAL with the given SQLSTATE
// code and message) so the client shows a clear error instead of "server closed
// the connection unexpectedly".
func sendError(conn net.Conn, code, message string) {
	var fields []byte
	add := func(typ byte, val string) {
		fields = append(fields, typ)
		fields = append(fields, val...)
		fields = append(fields, 0)
	}
	add('S', "FATAL")
	add('C', code)
	add('M', message)
	fields = append(fields, 0) // terminator

	out := make([]byte, 5+len(fields))
	out[0] = 'E'
	binary.BigEndian.PutUint32(out[1:], uint32(4+len(fields)))
	copy(out[5:], fields)
	_, _ = conn.Write(out)
}

func parseParams(b []byte) map[string]string {
	var parts []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == 0 {
			parts = append(parts, string(b[start:i]))
			start = i + 1
		}
	}
	params := map[string]string{}
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "" {
			break
		}
		params[parts[i]] = parts[i+1]
	}
	return params
}

func buildStartup(params map[string]string) []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, codeStartup30)
	for k, v := range params {
		body = append(body, k...)
		body = append(body, 0)
		body = append(body, v...)
		body = append(body, 0)
	}
	body = append(body, 0) // final terminator
	out := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(out, uint32(4+len(body)))
	copy(out[4:], body)
	return out
}

// reaper periodically suspends branches idle (no proxy activity and no active
// connections) for longer than idle.
func reaper(idle time.Duration) {
	interval := idle / 3
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	for {
		time.Sleep(interval)
		names, err := branch.SuspendableBranches()
		if err != nil {
			continue
		}
		for _, n := range names {
			mu.Lock()
			last, seen := lastActivity[n]
			if !seen {
				lastActivity[n] = time.Now() // first sight: start the idle clock
				mu.Unlock()
				continue
			}
			idleFor := time.Since(last)
			mu.Unlock()
			if idleFor < idle {
				continue
			}
			active, err := branch.ActiveConnections(n)
			if err != nil || active > 0 {
				continue // in use (or unreachable) — leave it running
			}
			log.Printf("auto-suspend: %s idle %s, 0 connections -> stopping", n, idleFor.Round(time.Second))
			if err := branch.Suspend(n); err != nil {
				log.Printf("suspend %s: %v", n, err)
			}
		}
	}
}
