package api

// usage.go —— 每租户的**用量读数与配额拦截**。
//
// 隔离管"谁看得见"，配额管"谁能用多少"。共享实例上只做隔离是不够的：一个循环打
// 接口的脚本就能把 LLM 预算烧光。这里给出两件事：
//
//   - GET /v1/usage：调用者 realm 的用量与配额读数（进程内口径，见 usage 包注释）
//   - 问答/生成前的配额检查：超限 → 429 + **可读原因**（哪个 realm、哪个额度、何时重置）
//
// 口径诚实：token 不知道就说不知道（`tokens_known=false`），配额是"每天"的
// （跨日自动清零），都不假装成账务系统。

import (
	"net/http"

	"github.com/willove/cumulus/internal/usage"
)

// Meter 是计量器（可选件）：nil = 不计量也不限流（本地开发形态）。
var _ = usage.Meter{}

// handleUsage 是用量读数面（GET /v1/usage）。
//
// 只报**调用者自己的 realm**——多租户下别人的用量不是你的数据（哪怕管理员另说）。
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.Meter == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"metered": false,
			"note":    "计量器未装（本地开发形态）：没有用量读数，也没有配额",
		})
		return
	}
	realm := s.realmOf(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"metered": true,
		"scope":   "process（进程内统计，重启归零；不是账务系统）",
		"usage":   s.Meter.Snapshot(realm),
	})
}

// allowQuota 是问答/生成前的配额闸门：超限 → 429。
//
// 顺序很重要：**先检查再记账**（usage.Exceeded 的纪律）。超限的那一次不该被计数，
// 否则一次拒绝会让剩余额度更快耗尽——那是"惩罚被限流的人"。
func (s *Server) allowQuota(w http.ResponseWriter, realm string, kind usage.Kind) bool {
	if s.Meter == nil {
		return true
	}
	if over, why := s.Meter.Exceeded(realm, kind); over {
		w.Header().Set("Retry-After", "3600")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error":  "quota exceeded",
			"realm":  realm,
			"detail": why,
			"usage":  s.Meter.Snapshot(realm),
		})
		return false
	}
	s.Meter.Record(realm, kind)
	return true
}
