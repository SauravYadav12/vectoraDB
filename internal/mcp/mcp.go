// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcp exposes VectoraDB's agent-branch operations over the Model Context
// Protocol (MCP), so an AI agent framework can — through one standard interface —
// get its own disposable database, run SQL, see exactly what it changed (from
// the tamper-evident schema ledger), and throw the database away.
//
// It speaks MCP over stdio: newline-delimited JSON-RPC 2.0 on stdin/stdout.
// stdout carries the protocol, so all logging goes to stderr.
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/vectoradb/vectoradb/internal/branch"
	"github.com/vectoradb/vectoradb/internal/version"
)

const protocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the MCP server on stdio until stdin closes.
func Serve() error {
	log.SetOutput(os.Stderr)
	log.SetPrefix("vdb mcp: ")
	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	out := bufio.NewWriter(os.Stdout)
	enc := json.NewEncoder(out)

	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			log.Printf("decode: %v", err)
			continue
		}
		resp, respond := dispatch(req)
		if !respond {
			continue // a notification gets no response
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if err := out.Flush(); err != nil {
			return err
		}
	}
}

func dispatch(req rpcRequest) (rpcResponse, bool) {
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "vectoradb", "version": version.Version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		resp.Result = callTool(req.Params)
	default:
		if len(req.ID) != 0 {
			resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
		}
	}
	// Requests carry an id and get a response; notifications (no id) do not.
	if len(req.ID) == 0 {
		return resp, false
	}
	return resp, true
}

func tool(name, desc string, props map[string]any, required []string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"name":        name,
		"description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": props, "required": required},
	}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func toolList() []map[string]any {
	return []map[string]any{
		tool("create_branch",
			"Create a fresh, isolated Postgres database branch for an agent (a copy-on-write clone of main) and return its connection string (DSN).",
			map[string]any{"agent_id": str("identifier for the agent")}, []string{"agent_id"}),
		tool("list_branches", "List the active agent branches and their DSNs.", nil, nil),
		tool("delete_branch", "Delete an agent's branch and all of its data.",
			map[string]any{"agent_id": str("identifier for the agent")}, []string{"agent_id"}),
		tool("run_sql", "Run SQL on a branch (default 'main') and return the result.",
			map[string]any{
				"branch": str("branch name (default main)"),
				"sql":    str("the SQL to run"),
			}, []string{"sql"}),
		tool("changes",
			"Show recent schema changes (DDL) on a branch — who changed what, when, and with which tool — from the tamper-evident ledger.",
			map[string]any{
				"branch": str("branch name (default main)"),
				"limit":  map[string]any{"type": "integer", "description": "max rows (default 50)"},
			}, nil),
		tool("verify_ledger",
			"Verify a branch's schema ledger has not been tampered with (recomputes the hash chain).",
			map[string]any{"branch": str("branch name (default main)")}, nil),
	}
}

func callTool(params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(params, &p)
	text, err := runTool(p.Name, p.Arguments)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func runTool(name string, args json.RawMessage) (string, error) {
	switch name {
	case "create_branch":
		var a struct {
			AgentID string `json:"agent_id"`
		}
		_ = json.Unmarshal(args, &a)
		if a.AgentID == "" {
			return "", fmt.Errorf("agent_id is required")
		}
		info, err := branch.CreateAgentBranch(a.AgentID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Branch %q is ready — a disposable Postgres isolated from main.\nDSN: %s", info.Branch, info.DSN), nil

	case "list_branches":
		infos, err := branch.ListAgentBranches()
		if err != nil {
			return "", err
		}
		if len(infos) == 0 {
			return "No agent branches.", nil
		}
		var b strings.Builder
		for _, i := range infos {
			fmt.Fprintf(&b, "%s\t%s\n", i.Branch, i.DSN)
		}
		return strings.TrimRight(b.String(), "\n"), nil

	case "delete_branch":
		var a struct {
			AgentID string `json:"agent_id"`
		}
		_ = json.Unmarshal(args, &a)
		if a.AgentID == "" {
			return "", fmt.Errorf("agent_id is required")
		}
		if err := branch.DeleteAgentBranch(a.AgentID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Deleted the branch for agent %q.", a.AgentID), nil

	case "run_sql":
		var a struct {
			Branch string `json:"branch"`
			SQL    string `json:"sql"`
		}
		_ = json.Unmarshal(args, &a)
		if strings.TrimSpace(a.SQL) == "" {
			return "", fmt.Errorf("sql is required")
		}
		return branch.QueryText(a.Branch, a.SQL)

	case "changes":
		var a struct {
			Branch string `json:"branch"`
			Limit  int    `json:"limit"`
		}
		_ = json.Unmarshal(args, &a)
		return branch.LedgerText(a.Branch, a.Limit)

	case "verify_ledger":
		var a struct {
			Branch string `json:"branch"`
		}
		_ = json.Unmarshal(args, &a)
		return branch.LedgerVerify(a.Branch)

	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}
