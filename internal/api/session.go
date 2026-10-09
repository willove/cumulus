package api

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	gocontext "context"

	"github.com/willove/cumulus/internal/store"

	"github.com/willove/cumulus/internal/harness"
)

// 会话层三个端点：把事件流变成**可回放、可断线恢复**的东西。
//
// 为什么需要（真跑才暴露的现实）：一次问答常在十几秒到几十秒，用户在移动网络上
// 会断线、切后台被挂起、刷新页面。只发一次 live 流，这些情况下**过程就丢了**——
// 答案还能重问，已经流出的进度/思考/正文不能重建。
//
//	POST /v1/sessions/{id}/events?cursor=N   → 补页（SSE，帧形状与实时**完全一致**）
//	GET  /v1/sessions/{id}                   → 清单（到哪了、完了吗）
//	GET  /v1/sessions                        → 会话列表
//
// 客户端因此只需要**一套解析器**：实时与回放同形，断线重连就是"拿 cursor 补页"。
const sessionPrefix = "/v1/sessions"

// handleSession 路由三个会话端点。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, sessionPrefix)
	path = strings.Trim(path, "/")
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.listSessions(w, r)
	case path != "" && r.Method == http.MethodGet && strings.HasSuffix(path, "/events"):
		s.replayEvents(w, r, strings.TrimSuffix(path, "/events"))
	case path != "" && r.Method == http.MethodGet:
		s.sessionManifest(w, r, path)
	case path != "" && r.Method == http.MethodDelete:
		// 用户显式删除（"清除记录"）。到期清理由后台清扫走同一条 PruneSession。
		n, err := harness.PruneSession(r.Context(), s.Store, path)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "prune failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": path, "deleted_events": n})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "unsupported session request")
	}
}

// sessionManifest 返回一场会话的清单。
func (s *Server) sessionManifest(w http.ResponseWriter, r *http.Request, id string) {
	man, ok := harness.SessionManifestOf(r.Context(), s.Store, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "session not found: "+id)
		return
	}
	writeJSON(w, http.StatusOK, man)
}

// listSessions 列出已知会话（清单集合按 id 前缀查不了，就逐个试探代价太大——
// 这里只报"已知 id 列表"，由调用方从业务侧传入自己关心的 id；这是刻意的
// 保守：没有索引支持的全集扫描，就不假装能给全集）。
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	ids := r.URL.Query()["id"]
	if len(ids) == 0 {
		// **不带 id 时列出库里全部会话**（按保留期排序，最新在前）。
		//
		// 原来只有 `?id=` 一种查法，于是"我这次会话有哪些"根本问不出来——
		// 验收脚本第一次跑就撞在这里（"没有可用 session"其实是我**查不出**，
		// 不是没有）。清单类接口不给"列出来"的能力，调用方就只能自己记 id。
		if s.Store == nil {
			writeJSON(w, http.StatusOK, map[string]any{"sessions": []harness.SessionManifest{}})
			return
		}
		ids, _ = s.Store.ListIDs(r.Context(), harness.SessionManifestCollection, 0)
	}
	out := make([]harness.SessionManifest, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if man, ok := harness.SessionManifestOf(r.Context(), s.Store, id); ok {
			out = append(out, man)
		}
	}
	// 新的在前（UpdatedAt 是字符串时间戳，字典序即时间序；缺失时保持库序）
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// replayEvents 补页：把 seq > cursor 的事件按**与实时完全相同**的帧形状发出去。
//
// 同一个 SSE 编码器、同一批构造器——不是"回放专用格式"。客户端一套解析器通吃，
// 这是少写一整套状态机的关键（真跑踩过两套格式的分歧：回放少了 thinking 通道，
// 重连后思考区整个不出现）。
func (s *Server) replayEvents(w http.ResponseWriter, r *http.Request, id string) {
	cursor := atoiDefault(r.URL.Query().Get("cursor"), 0)
	limit := atoiDefault(r.URL.Query().Get("limit"), 0)
	evs, man, err := harness.Replay(r.Context(), s.Store, id, cursor, limit)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported by this server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	stream := harness.NewSSE(w, flusher.Flush)
	for _, ev := range evs {
		_ = stream.Write(ev)
	}
	// 回放头一帧告诉客户端"补到哪儿了"——不靠猜（cursor 回退或缺口都要看得见）。
	hdr, herr := harness.Started(id, fmt.Sprintf("replay: %d/%d（最后序号 %d，%s）",
		len(evs), man.Count, man.LastSeq, completeWord(man.Complete)))
	if herr == nil {
		_ = stream.Write(hdr)
	}
	_ = stream.Done()
	flusher.Flush()
}

func completeWord(complete bool) string {
	if complete {
		return "已收尾"
	}
	return "中途断开"
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// sessionTTL 读会话保留期（CUMULUS_SESSION_TTL，如 `72h`；0/未设 = 默认 7 天）。
//
// 落库的是**用户提问原文 + 引用原文 + 思考过程**——不是该永久保存的东西。
func sessionTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("CUMULUS_SESSION_TTL"))
	if v == "" {
		return harness.DefaultSessionTTL
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return harness.DefaultSessionTTL // 配错不静默放宽
	}
	return d
}

// pruneSessions 清掉已过期的会话（serve 定期调用 + 启动时清一次）。
// 这是**唯一**该跑在后台的删除路径：到期就删，不等谁来点。
func PruneSessions(ctx gocontext.Context, st store.Port) {
	sessions, events, err := harness.PruneExpired(ctx, st, time.Now())
	if err != nil {
		fmt.Printf("session: 清理过期会话出错（不阻断服务）：%v\n", err)
		return
	}
	if sessions > 0 {
		fmt.Printf("session: 清理过期会话 %d 场（%d 帧）\n", sessions, events)
	}
}
