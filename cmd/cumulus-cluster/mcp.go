package main

// MCP face (P8): POST /mcp speaks JSON-RPC 2.0 with the three tools the
// agent ecosystem consumes — search / list_clusters / get_cluster — over the
// SAME stack as the REST faces (newSearchStack, cluster stores), so a tool
// call and its REST twin can never drift. Transport note: Streamable HTTP
// clients POST one message and read one JSON-RPC response; notifications
// (no id) are accepted and answered 202 with an empty body.
//
// Local MCP clients (Claude Desktop, Cursor, …) speak stdio, and a Badger
// directory holds an exclusive lock — a second process opening the same
// store would fail. So the stdio side is a thin proxy (mcpcli.go) that
// forwards to a running serve's /mcp; this face itself opens no store.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/deep"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/ns"
)

func envVerbose() bool { return os.Getenv("CLUS_VERBOSE") == "1" }

// mcpProtocolVersion is the MCP revision this face implements.
const mcpProtocolVersion = "2025-03-26"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent/null = notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func (r rpcRequest) isNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpToolDef is one advertised tool: name, description, JSON Schema, handler.
type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpToolList() []mcpToolDef {
	return []mcpToolDef{
		{
			Name:        "search",
			Description: "对私域语料做一次检索问答：返回答案（带引用编号）、证据窗口、置信度与档位。证据可回溯到原文。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   map[string]any{"type": "string", "description": "自然语言问题"},
					"ns":      map[string]any{"type": "string", "description": "命名空间（多租户分域）；缺省用 serve 级 -ns"},
					"session": map[string]any{"type": "string", "description": "会话 ID（多轮上下文折叠，可选）"},
					"prior":   map[string]any{"type": "boolean", "description": "五信号先验融合排序（默认关，与 CLI/REST 一致）"},
					"l1pre":   map[string]any{"type": "boolean", "description": "内容向量 KNN 收窄候选（索引缺席时自动跳过）"},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "list_clusters",
			Description: "列出知识簇（自进化记忆）：名称、问法集、生命周期、版本、置信度、热度。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ns":        map[string]any{"type": "string", "description": "命名空间；缺省用 serve 级 -ns"},
					"limit":     map[string]any{"type": "integer", "description": "返回条数上限（1..1000，默认 100）"},
					"lifecycle": map[string]any{"type": "string", "description": "按生命周期过滤：emerging/stable/contested/deprecated"},
				},
			},
		},
		{
			Name:        "get_cluster",
			Description: "取一个知识簇的详情：内容、问法集、版本/置信/热度，以及该簇的 cites 证据边（簇→源证据窗口）。",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "簇 ID"},
					"ns": map[string]any{"type": "string", "description": "命名空间；缺省用 serve 级 -ns"},
				},
				"required": []string{"id"},
			},
		},
	}
}

// registerMCPFace mounts POST /mcp. Same ctx/store wiring as the REST faces:
// serve-level namespace, per-request "ns" override, no store of its own.
func registerMCPFace(mux *http.ServeMux, c cumulite.Port, st *ingest.Store, sourcesColl, serveNS string, verbose bool, ensure *nsEnsurer) {
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
			return
		}
		var req rpcRequest
		if err := decode(r, &req); err != nil {
			writeRPC(w, rpcResponse{
				JSONRPC: "2.0", ID: nil,
				Error: &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
			})
			return
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			writeRPC(w, rpcResponse{
				JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32600, Message: "invalid jsonrpc version"},
			})
			return
		}
		if req.isNotification() {
			w.WriteHeader(http.StatusAccepted) // notifications get no response
			return
		}
		resp := mcpDispatch(r.Context(), c, st, sourcesColl, serveNS, verbose, req, ensure)
		writeRPC(w, resp)
	})
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func rpcErr(id json.RawMessage, code int, format string, a ...any) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: fmt.Sprintf(format, a...)}}
}

// mcpDispatch routes one JSON-RPC request. Tool execution failures come back
// as a tool result with isError=true (MCP convention); protocol failures as
// JSON-RPC errors.
func mcpDispatch(ctx context.Context, c cumulite.Port, st *ingest.Store, sourcesColl, serveNS string, verbose bool, req rpcRequest, ensure *nsEnsurer) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "cumulus-cluster", "version": "1"},
		}}
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": mcpToolList()}}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return rpcErr(req.ID, -32602, "invalid params: %v", err)
		}
		text, isErr := mcpCallTool(ctx, c, st, sourcesColl, serveNS, verbose, p.Name, p.Arguments, ensure)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		}}
	default:
		return rpcErr(req.ID, -32601, "method not found: %s", req.Method)
	}
}

// mcpCallTool runs one tool and returns its text payload (JSON) plus the
// isError flag. Every tool honors the per-request "ns" like its REST twin.
func mcpCallTool(ctx context.Context, c cumulite.Port, st *ingest.Store, sourcesColl, serveNS string, verbose bool, name string, args map[string]any, ensure *nsEnsurer) (string, bool) {
	argStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	reqNS := argStr("ns")
	if err := ns.Validate(reqNS); err != nil {
		return mcpErrJSON("invalid ns: " + err.Error()), true
	}
	nsForReq := firstNonEmpty(reqNS, serveNS)

	switch name {
	case "search":
		query := argStr("query")
		if query == "" {
			return mcpErrJSON("query required"), true
		}
		stForReq, sourcesForReq, err := storeForNS(c, st, serveNS, reqNS, sourcesColl)
		if err != nil {
			return mcpErrJSON(err.Error()), true
		}
		if eerr := ensure.declare(ctx, nsForReq); eerr != nil {
			return mcpErrJSON(fmt.Sprintf("ensure ns %q: %v", nsForReq, eerr)), true
		}
		opt := SearchOptions{
			Prior:     argBool(args, "prior", false),
			L1Pre:     argBool(args, "l1pre", false),
			Namespace: nsForReq,
		}
		var sess *sessionStore
		if sid := argStr("session"); sid != "" {
			sess = &sessionStore{c: c, ns: nsForReq}
			hist, _, herr := sessionHistory(ctx, *sess, sid, 6)
			if herr != nil {
				return mcpErrJSON("session: " + herr.Error()), true
			}
			opt.History = hist
		}
		ss, err := newSearchStack(ctx, c, stForReq, sourcesForReq, opt)
		if err != nil {
			return mcpErrJSON(err.Error()), true
		}
		if verbose || envVerbose() {
			ss.dE.Verbose = func(f string, a ...any) { log.Printf("[mcp search %s] %s", query, fmt.Sprintf(f, a...)) }
		}
		res, err := runSearch(ctx, ss, query)
		if err != nil {
			return mcpErrJSON(err.Error()), true
		}
		if sess != nil {
			if _, aerr := sess.appendTurn(ctx, argStr("session"), query, query, res.Answer.Summary); aerr == nil {
				res.Session = argStr("session")
			}
		}
		return mcpJSON(res), false

	case "list_clusters":
		store := cluster.NewCumuStore(c, ns.Coll(nsForReq, "clus_clusters"))
		all, err := store.All(ctx)
		if err != nil {
			return mcpErrJSON(err.Error()), true
		}
		sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
		if want := argStr("lifecycle"); want != "" {
			filtered := all[:0]
			for _, cl := range all {
				if cl.Lifecycle == want {
					filtered = append(filtered, cl)
				}
			}
			all = filtered
		}
		limit := 100
		if v, ok := args["limit"].(float64); ok && v > 0 && v <= 1000 {
			limit = int(v)
		}
		if len(all) > limit {
			all = all[:limit]
		}
		return mcpJSON(map[string]any{"namespace": nsForReq, "clusters": all}), false

	case "get_cluster":
		id := argStr("id")
		if id == "" {
			return mcpErrJSON("id required"), true
		}
		cl, err := cluster.NewCumuStore(c, ns.Coll(nsForReq, "clus_clusters")).Get(ctx, id)
		if err != nil {
			return mcpErrJSON(err.Error()), true
		}
		if cl == nil {
			return mcpErrJSON("cluster not found: " + id), true
		}
		cites := []map[string]any{}
		if all, cerr := deep.NewCumuCiteStore(c, ns.Coll(nsForReq, "clus_cites")).List(ctx); cerr == nil {
			for _, e := range all {
				if from, _ := e["_from"].(string); from == id {
					cites = append(cites, e)
				}
			}
		}
		return mcpJSON(map[string]any{"namespace": nsForReq, "cluster": cl, "cites": cites}), false

	default:
		return mcpErrJSON("unknown tool: " + name), true
	}
}

func argBool(args map[string]any, k string, def bool) bool {
	if v, ok := args[k].(bool); ok {
		return v
	}
	return def
}

func mcpJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return mcpErrJSON(err.Error())
	}
	return string(b)
}

func mcpErrJSON(msg string) string {
	b, _ := json.Marshal(map[string]any{"error": msg})
	return string(b)
}
