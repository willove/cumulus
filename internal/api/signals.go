package api

// signals.go —— 使用信号面：记信号、聚合计数，以及**学习层的观察**
// （learncore.ObserveUsage）。信号此前只被计数、没人消费；这里让它变成可执行读数。

import (
	"net/http"

	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/learncore"
)

func (s *Server) handleSignals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.Signals == nil {
		writeErr(w, http.StatusNotImplemented, "signal store not configured")
		return
	}
	// 学习层的观察（此前信号只被计数、没人消费）：把线上信号折成可执行读数。
	// 只观察不推断——提议仍要过 Hypothesizer 的白名单。
	usage, uerr := learncore.ObserveUsage(s.Signals, 10)
	if uerr != nil {
		writeErr(w, http.StatusInternalServerError, "usage observation: "+uerr.Error())
		return
	}
	out := map[string]any{
		"usage":   usage,
		"summary": usage.Summary(),
		"total":   s.Signals.Len(),
		"counts":  s.Signals.Counts(),
		"top": map[string]any{
			knowledge.SignalReaskAfterRefusal: questionCounts(s.Signals.TopQuestions(knowledge.SignalReaskAfterRefusal, 10)),
			knowledge.SignalReaskAfterAnswer:  questionCounts(s.Signals.TopQuestions(knowledge.SignalReaskAfterAnswer, 10)),
			knowledge.SignalCitationClick:     citeCounts(s.Signals.TopCitations(10)),
		},
	}
	writeJSON(w, http.StatusOK, out)
}
