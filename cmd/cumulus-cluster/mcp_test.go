package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/ingest"
	"github.com/willove/cumulus/internal/source"
)

// MCP face gate (P8): the three tools run over the same stack as the REST
// faces, namespace scoping holds, and protocol/tool errors surface the MCP
// way (JSON-RPC error / isError result) — all against the embedded cumulite
// engine with no server process anywhere.
func TestMCPFaceToolsRoundTrip(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "") // offline gates: no network in tests
	ctx := context.Background()

	engine, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer engine.Close()

	st := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatalf("boot ensure: %v", err)
	}
	if _, err := st.Put(ctx, source.New("部署手册", "md", "", "handbook", "zh",
		"连接池最大连接数为 200，端口 8484。备份策略：每日全量保留 30 天。", nil)); err != nil {
		t.Fatalf("put: %v", err)
	}

	mux := http.NewServeMux()
	registerMCPFace(mux, engine, st, "clus_sources", "", false,
		newNSEnsurer(engine, st, "", "clus_sources"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// initialize handshake.
	init := mcpRPC(t, srv.URL, "initialize", nil)
	if init.Error != nil {
		t.Fatalf("initialize error: %+v", init.Error)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(init.Result, &initRes); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	if initRes.ProtocolVersion != mcpProtocolVersion || initRes.ServerInfo.Name != "cumulus-cluster" {
		t.Fatalf("initialize: %+v", initRes)
	}

	// tools/list advertises exactly the three planned tools.
	list := mcpRPC(t, srv.URL, "tools/list", nil)
	if list.Error != nil {
		t.Fatalf("tools/list error: %+v", list.Error)
	}
	var toolRes struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(list.Result, &toolRes); err != nil {
		t.Fatalf("tools/list result: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range toolRes.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"search", "list_clusters", "get_cluster"} {
		if !names[want] {
			t.Fatalf("tools/list missing %q: %+v", want, toolRes.Tools)
		}
	}
	if len(toolRes.Tools) != 3 {
		t.Fatalf("tools/list want 3, got %d", len(toolRes.Tools))
	}

	// search tool: answer, mode and citations come back in one text payload.
	search := mcpRPC(t, srv.URL, "tools/call", map[string]any{
		"name":      "search",
		"arguments": map[string]any{"query": "连接池最大连接数"},
	})
	if search.Error != nil {
		t.Fatalf("search error: %+v", search.Error)
	}
	text, isErr := mcpToolText(t, search)
	if isErr {
		t.Fatalf("search isError: %s", text)
	}
	var res struct {
		Answer struct {
			Summary string `json:"summary"`
		} `json:"answer"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("search payload: %v (%s)", err, text)
	}
	if !strings.Contains(res.Answer.Summary, "200") || res.Mode == "" {
		t.Fatalf("search payload shape: %s", text)
	}

	// list_clusters: the search just persisted one cluster.
	list2 := mcpRPC(t, srv.URL, "tools/call", map[string]any{
		"name":      "list_clusters",
		"arguments": map[string]any{},
	})
	text, isErr = mcpToolText(t, list2)
	if isErr {
		t.Fatalf("list isError: %s", text)
	}
	var clusters struct {
		Clusters []struct {
			ID string `json:"_id"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(text), &clusters); err != nil {
		t.Fatalf("list payload: %v (%s)", err, text)
	}
	if len(clusters.Clusters) == 0 {
		t.Fatalf("list_clusters empty after search: %s", text)
	}
	clusterID := clusters.Clusters[0].ID

	// get_cluster: detail carries cites.
	get := mcpRPC(t, srv.URL, "tools/call", map[string]any{
		"name":      "get_cluster",
		"arguments": map[string]any{"id": clusterID},
	})
	text, isErr = mcpToolText(t, get)
	if isErr {
		t.Fatalf("get isError: %s", text)
	}
	var detail struct {
		Cluster struct {
			ID string `json:"_id"`
		} `json:"cluster"`
		Cites []map[string]any `json:"cites"`
	}
	if err := json.Unmarshal([]byte(text), &detail); err != nil {
		t.Fatalf("get payload: %v (%s)", err, text)
	}
	if detail.Cluster.ID != clusterID {
		t.Fatalf("get_cluster id mismatch: %s", text)
	}
}

func TestMCPFaceErrorsAndNamespaces(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "")
	ctx := context.Background()

	engine, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	defer engine.Close()

	st := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(ctx, suiteExtra("")...); err != nil {
		t.Fatalf("boot ensure: %v", err)
	}
	if _, err := st.Put(ctx, source.New("t9 手册", "md", "", "h9", "zh", "租户 t9 的连接池上限是 64。", nil)); err != nil {
		t.Fatalf("put: %v", err)
	}

	mux := http.NewServeMux()
	registerMCPFace(mux, engine, st, "clus_sources", "", false,
		newNSEnsurer(engine, st, "", "clus_sources"))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Protocol errors: unknown method and unknown tool.
	unknown := mcpRPC(t, srv.URL, "no/such.method", nil)
	if unknown.Error == nil || unknown.Error.Code != -32601 {
		t.Fatalf("unknown method: %+v", unknown)
	}
	badTool := mcpRPC(t, srv.URL, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	text, isErr := mcpToolText(t, badTool)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("unknown tool: %s", text)
	}
	// Missing required argument: tool-level error, not a protocol error.
	noQuery := mcpRPC(t, srv.URL, "tools/call", map[string]any{"name": "search", "arguments": map[string]any{}})
	if _, isErr := mcpToolText(t, noQuery); !isErr {
		t.Fatalf("missing query must be a tool error")
	}

	// A fresh namespace is declared on first tool use (fail-closed engine),
	// search lands, and the cluster stays invisible to the default library.
	scoped := mcpRPC(t, srv.URL, "tools/call", map[string]any{
		"name":      "search",
		"arguments": map[string]any{"query": "t9 连接池上限", "ns": "t9"},
	})
	text, isErr = mcpToolText(t, scoped)
	if isErr {
		t.Fatalf("scoped search isError: %s", text)
	}
	list := mcpRPC(t, srv.URL, "tools/call", map[string]any{
		"name":      "list_clusters",
		"arguments": map[string]any{"ns": "t9"},
	})
	text, _ = mcpToolText(t, list)
	if !strings.Contains(text, "t9") {
		t.Fatalf("t9 list payload: %s", text)
	}
	listDef := mcpRPC(t, srv.URL, "tools/call", map[string]any{
		"name":      "list_clusters",
		"arguments": map[string]any{},
	})
	text, _ = mcpToolText(t, listDef)
	if strings.Contains(text, "t9 手册") {
		t.Fatalf("default library leaked tenant cluster: %s", text)
	}

	// Notifications get 202 and no body (stdio proxy relies on this).
	resp, err := http.Post(srv.URL+"/mcp", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("notification post: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || len(bytes.TrimSpace(body)) != 0 {
		t.Fatalf("notification: status=%d body=%q", resp.StatusCode, body)
	}
}

// --- helpers -----------------------------------------------------------

type rpcTestResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func mcpRPC(t *testing.T, url, method string, params any) rpcTestResponse {
	t.Helper()
	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		payload["params"] = json.RawMessage(raw)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(url+"/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", method, err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read %s: %v", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s status %d: %s", method, resp.StatusCode, raw)
	}
	var out rpcTestResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", method, err, raw)
	}
	return out
}

// mcpToolText extracts the text content and isError flag from a tools/call
// result.
func mcpToolText(t *testing.T, resp rpcTestResponse) (string, bool) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("tools/call protocol error: %+v", resp.Error)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("tools/call result: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("tools/call: no content")
	}
	return res.Content[0].Text, res.IsError
}
