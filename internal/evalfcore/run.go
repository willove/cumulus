package evalfcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Status 是运行状态。
type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusDone        Status = "done"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
)

// ErrStartNewRun 表示这个 runID 的历史状态不能续跑，必须另起新 run。
var ErrStartNewRun = errors.New("evalfcore: start a new run")

// RunState 与逐题结果在同一个 KV 值里原子落盘——进度不可能领先于
// 未持久化的结果。进程被 SIGKILL 后：已完成的运行原样还在；当时在飞的
// 运行重启后只报 interrupted（绝不冷启动重放，那是没被同意的二次计费）。
type RunState struct {
	RunID        string       `json:"run_id"`
	Status       Status       `json:"status"`
	Fingerprints Fingerprints `json:"fingerprints"`
	ItemsTotal   int          `json:"items_total"`
	ItemsDone    int          `json:"items_done"`
	Results      []ItemResult `json:"results"`
	Error        string       `json:"error,omitempty"`
}

// ItemResult 一条题的结果。JudgeOK 为 nil 表示未判分（N/A）——
// N/A 与 0 是两件事，界面与统计都不许把 N/A 当 0。
type ItemResult struct {
	ItemID            string   `json:"item_id"`
	Answer            string   `json:"answer"`
	RuleScore         float64  `json:"rule_score"`
	EvidenceHit       bool     `json:"evidence_hit"`
	CitationsResolved int      `json:"citations_resolved"`
	CitationsTotal    int      `json:"citations_total"`
	CitedDocs         []string `json:"cited_docs,omitempty"` // 信念观测的原料：哪些文档被引用了
	JudgeOK           *bool    `json:"judge_ok,omitempty"`
	JudgeTokens       int      `json:"judge_tokens,omitempty"` // 判官花费（prompt+completion），进账单
	Failure           string   `json:"failure,omitempty"`
	LatencyMS         int64    `json:"latency_ms"`
	PromptTokens      int      `json:"prompt_tokens"`
	CompletionTokens  int      `json:"completion_tokens"`
	CostKnown         bool     `json:"cost_known"`
}

// Citation 是答案里的一条引用。Resolved 为假 = 坐标回溯失败。
type Citation struct {
	DocID    string
	Span     string
	Resolved bool
}

// ItemOutcome 是 executor 对一道题的产出。看不到金标（见包注释）。
type ItemOutcome struct {
	Answer           string
	Cited            []Citation
	Refused          bool
	RouteAction      string
	Windows          int
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
}

// Executor 在冻结语料上执行一次问答。实现方负责隔离：每次运行一个
// 独立引擎实例，灌冻结语料原样（调用方把语料喂给实现）。
type Executor interface {
	Answer(ctx context.Context, question string) (ItemOutcome, error)
}

// RunStore 运行档案。一个 run 一个 key = 原子写（store.Port 的 KV 语义）。
type RunStore interface {
	SaveRun(ctx context.Context, state RunState) error
	LoadRun(ctx context.Context, runID string) (RunState, error)
}

// ErrRunNotFound 档案不存在。
var ErrRunNotFound = errors.New("evalfcore: run not found")

// Runner 跑一次 episode。零值不可用，用 NewRunner。
type Runner struct {
	Store        RunStore
	Fingerprints Fingerprints
	Executor     Executor
	Judge        Judge // 可空：空则判官臂记 N/A
}

// NewRunner 组装。指纹与执行面对外可见以便审计。
func NewRunner(store RunStore, fp Fingerprints, ex Executor, j Judge) *Runner {
	return &Runner{Store: store, Fingerprints: fp, Executor: ex, Judge: j}
}

// Start 开始（或认领）一次运行：
//   - 新 runID：排队 → 逐题执行 → 每完成一题原子落盘 → 全部完成记 done；
//   - 已有 running 档案：不重跑，标记 interrupted 后返回既有档案；
//   - 已有 done/failed/interrupted 档案：返回 ErrStartNewRun。
//
// executor 出错：该运行记 failed，已完成的题留在档案里，错误带上
// 已落盘的题数（可审计的中断点）。
func (r *Runner) Start(ctx context.Context, runID string, items []Item) (RunState, error) {
	if prev, err := r.Store.LoadRun(ctx, runID); err == nil {
		switch prev.Status {
		case StatusRunning:
			prev.Status = StatusInterrupted
			prev.Error = "interrupted: process died mid-run; not cold-replayed"
			if serr := r.Store.SaveRun(ctx, prev); serr != nil {
				return prev, fmt.Errorf("evalfcore: mark interrupted: %w", serr)
			}
			return prev, nil
		case StatusDone, StatusFailed, StatusInterrupted:
			return prev, fmt.Errorf("run %s is %s: %w", runID, prev.Status, ErrStartNewRun)
		}
	} else if !errors.Is(err, ErrRunNotFound) {
		return RunState{}, fmt.Errorf("evalfcore: load prior state: %w", err)
	}

	state := RunState{
		RunID:        runID,
		Status:       StatusRunning,
		Fingerprints: r.Fingerprints,
		ItemsTotal:   len(items),
	}
	if err := r.Store.SaveRun(ctx, state); err != nil {
		return RunState{}, fmt.Errorf("evalfcore: init state: %w", err)
	}

	for i, item := range items {
		if err := ctx.Err(); err != nil {
			state.Status = StatusInterrupted
			state.Error = "context cancelled"
			_ = r.Store.SaveRun(ctx, state)
			return state, nil
		}
		res, err := r.runItem(ctx, item)
		if err != nil {
			state.Status = StatusFailed
			state.Error = fmt.Sprintf("item %s: %v", item.ID, err)
			_ = r.Store.SaveRun(ctx, state)
			return state, fmt.Errorf("evalfcore: item %s: %w (completed %d persisted)", item.ID, err, state.ItemsDone)
		}
		state.Results = append(state.Results, res)
		state.ItemsDone = i + 1
		// 每完成一题原子落盘：进度永远不领先于结果
		if err := r.Store.SaveRun(ctx, state); err != nil {
			return state, fmt.Errorf("evalfcore: persist after item %s: %w", item.ID, err)
		}
	}
	state.Status = StatusDone
	if err := r.Store.SaveRun(ctx, state); err != nil {
		return state, fmt.Errorf("evalfcore: persist done: %w", err)
	}
	return state, nil
}

// runItem 执行一题并算分。金标只在 runner 侧可见：executor 拿不到它。
func (r *Runner) runItem(ctx context.Context, item Item) (ItemResult, error) {
	start := time.Now()
	out, err := r.Executor.Answer(ctx, item.Question)
	if err != nil {
		return ItemResult{}, err
	}
	latency := out.LatencyMS
	if latency == 0 {
		latency = time.Since(start).Milliseconds()
	}

	var citedIDs []string
	resolvedCount := 0
	for _, c := range out.Cited {
		citedIDs = append(citedIDs, c.DocID)
		if c.Resolved {
			resolvedCount++
		}
	}

	res := ItemResult{
		ItemID:            item.ID,
		CitedDocs:         citedIDs,
		Answer:            out.Answer,
		RuleScore:         RuleScore(out.Answer, item.Answer),
		EvidenceHit:       EvidenceHit(citedIDs, item.GoldIDs),
		CitationsResolved: resolvedCount,
		CitationsTotal:    len(out.Cited),
		LatencyMS:         latency,
		PromptTokens:      out.PromptTokens,
		CompletionTokens:  out.CompletionTokens,
		CostKnown:         out.CostKnown,
	}
	if r.Judge != nil {
		v, jerr := r.Judge.Judge(item.Question, out.Answer, item.Answer)
		if jerr == nil {
			ok := v.OK
			res.JudgeOK = &ok
			res.JudgeTokens = v.PromptTokens + v.CompletionTokens
		}
	}
	unresolved := len(out.Cited) - resolvedCount
	// 失败标签只出现在真失败的题上：答错、没命中证据、引用核不掉、拒答，
	// 四者占一即失败。全过的题 Failure 为空——通过项不许挂失败标签
	// （否则学习周期的诊断会把全对当成“有可学”）。
	failed := res.RuleScore < 1 || !res.EvidenceHit || unresolved > 0 || out.Refused
	if failed {
		res.Failure = FormatCategory(Classify(ClassifyInput{
			Windows:             out.Windows,
			GoldHit:             res.EvidenceHit,
			UnresolvedCitations: unresolved,
			Refused:             out.Refused,
			RouteAction:         out.RouteAction,
			RuleScore:           res.RuleScore,
		}))
	}
	return res, nil
}

// KV encode/decode helpers（cmd 侧的 RunStore 实现用）——放这里免得
// 每个实现各写一遍 JSON。
func EncodeState(s RunState) ([]byte, error) {
	buf, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("evalfcore: encode state: %w", err)
	}
	return buf, nil
}

func DecodeState(buf []byte) (RunState, error) {
	var s RunState
	if err := json.Unmarshal(buf, &s); err != nil {
		return RunState{}, fmt.Errorf("evalfcore: decode state: %w", err)
	}
	return s, nil
}
