package evalfcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/willove/cumulus/internal/failure"
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

// RunState 的逐题结果落文档集合、清单落 KV 小值，**清单最后写**——
// 进度不可能领先于未持久化的结果（见 archive.go）。进程被 SIGKILL 后：
// 已完成的运行原样还在；当时在飞的运行重启后只报 interrupted
// （绝不冷启动重放，那是没被同意的二次计费）。
type RunState struct {
	RunID        string       `json:"run_id"`
	Arm          string       `json:"arm,omitempty"` // 臂标签（bm25 / bm25+rerank）：A/B 里的变动因子，不进指纹——指纹冻结实验，臂是实验内的对照维度
	Status       Status       `json:"status"`
	Fingerprints Fingerprints `json:"fingerprints"`
	ItemsTotal   int          `json:"items_total"`
	ItemErrors   int          `json:"item_errors,omitempty"` // 单题失败数（不杀全场的那些）
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
	RerankApplied     bool     `json:"rerank_applied,omitempty"` // 语义重排是否生效
	RerankReason      string   `json:"rerank_reason,omitempty"`  // 未生效原因
	CitedDocs         []string `json:"cited_docs,omitempty"`     // 信念观测的原料：哪些文档被引用了
	CitedSpans        []string `json:"cited_spans,omitempty"`    // "<docID>#<span>"：人工/复核要拿到证据原文，只有 docid 不够
	Refused           bool     `json:"refused,omitempty"`        // 系统拒答（合法结局，不是崩溃）
	JudgeOK           *bool    `json:"judge_ok,omitempty"`
	JudgeCoverage     float64  `json:"judge_coverage,omitempty"` // 答案级验证信号（分点命中比例）
	JudgeErr          string   `json:"judge_err,omitempty"`      // 判官没判上分的原因（留痕，不许静默 N/A）
	JudgeTokens       int      `json:"judge_tokens,omitempty"`   // 判官花费（prompt+completion），进账单
	JudgeRaw          string   `json:"judge_raw,omitempty"`      // 判词原文（校准用）
	Failure           string   `json:"failure,omitempty"`
	RouteAction       string   `json:"route_action,omitempty"` // fast / escalate / refuse（归因与校准分桶用）
	Confidence        float64  `json:"confidence,omitempty"`   // 路由实际用的置信代理（校准分桶用）
	// 候选信号：校准要能**比较**信号，不能只有一个复合值。一次付费跑动
	// 同时收齐，之后在同一批结果上比单调性与 α 可行性（见 internal/calib）。
	Coverage         float64 `json:"coverage,omitempty"`         // 查询词覆盖度（语料内可达词口径）
	Margin           float64 `json:"margin,omitempty"`           // (top1-top2)/top1：候选区分度
	Support          float64 `json:"support,omitempty"`          // 答案的词面支持（SLC 离线代理，post-answer）
	SupportN         int     `json:"support_terms,omitempty"`    // 支持度分母（答案内容词数；0 = 无可验证断言）
	VerifyNoul       float64 `json:"verify_noul,omitempty"`      // 决策模型：答案事实点有依据（0..1）
	VerifyChoice     string  `json:"verify_choice,omitempty"`    // 决策模型：窗口四级 ANSWER/RELATED/OUTDATED/UNKNOWN
	VerifyConf       float64 `json:"verify_conf,omitempty"`      // 决策模型对该判断的置信度
	DecisionApplied  bool    `json:"decision_applied,omitempty"` // 闸门/决策层是否真的跑了
	DecisionReason   string  `json:"decision_reason,omitempty"`  // not-bound / error:… / ok
	GateBlocked      bool    `json:"gate_blocked,omitempty"`     // 闸门是否真的拦下了这一题
	DecisionNoul     float64 `json:"decision_noul,omitempty"`    // 闸门分
	RouteTier        string  `json:"route_tier,omitempty"`       // 置信信号档位：logprob / retrieval
	LatencyMS        int64   `json:"latency_ms"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostKnown        bool    `json:"cost_known"`
}

// Citation 是答案里的一条引用。Resolved 为假 = 坐标回溯失败。
type Citation struct {
	DocID    string
	Span     string
	Resolved bool
}

// ItemOutcome 是 executor 对一道题的产出。看不到金标（见包注释）。
type ItemOutcome struct {
	Answer        string
	RerankApplied bool   // 语义重排是否真的生效（可选组件审计）
	RerankReason  string // 没生效的原因（留痕：degraded 必须可见）
	Cited         []Citation
	Refused       bool
	RouteAction   string
	Coverage      float64 // 检索覆盖度（候选信号）
	Margin        float64 // 候选区分度（候选信号）
	Support       float64 // 答案词面支持（候选信号；post-answer）
	SupportN      int     // 支持度分母（0 = 答案没有可验证的内容词）
	// 决策模型的答案级判断（另一家族，天然非循环）：
	VerifyNoul   float64 // 答案事实点是否都在窗口原文里有依据（0..1）
	VerifyChoice string  // 窗口与问题的关系（GaRAGe 四级）
	VerifyConf   float64
	// 闸门/决策层留痕：有没有真决策、为什么、闸门分多少。没跑与
	// "跑了但判不行"在读数里必须分得开（§三·八 降级可见）。
	DecisionApplied  bool
	DecisionReason   string
	DecisionNoul     float64
	GateBlocked      bool // 闸门是否真的拦下了这一题
	Windows          int
	LatencyMS        int64
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
	// 底物信号（失败归因用，BioHarness）：窗里有没有可读数值、
	// 查询内容词是不是全体语料外。执行面手上有窗口与查询分析，顺手报
	// 上来；缺省 false = 按"不知道"处理（判据保守，宁漏勿滥）。
	EvidenceHasNumeric bool
	QueryOutOfCorpus   bool
	// Confidence 是本次路由实际用的置信代理（无 logprobs 时是检索侧
	// 合成值，档位见 RouteTier）。校准报告要用它分桶，所以必须随结果落盘。
	Confidence float64
	RouteTier  string
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
//   - 新 runID：排队 → 逐题执行 → 每完成一题落一条 + 更新清单 → 全部完成记 done；
//   - 已有 running 档案：不重跑，标记 interrupted 后返回既有档案；
//   - 已有 done/failed/interrupted 档案：返回 ErrStartNewRun。
//
// executor 出错：该运行记 failed，已完成的题留在档案里，错误带上
// 已落盘的题数（可审计的中断点）。
// systemicHead 是系统性故障判窗口：开头这么多次全部单题失败 → 端点/
// key 级故障，停。之后单题失败只记项（网络抖动、畸形输出各例）。
const systemicHead = 5

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
			// 单题失败不杀全场：记成失败项（带原因）继续跑——一次网络
			// 抖动或一个畸形模型输出不该废掉 12 分钟的评测（真跑教训：
			// 300 题两次都在 130-250 题处被单题杀死）。系统性故障另行
			// 判：前 systemicHead 题全错即停（key 错、端点挂，重试无意义）。
			state.Results = append(state.Results, ItemResult{
				ItemID:  item.ID,
				Refused: false,
				Failure: "eval-error: " + err.Error(),
			})
			state.ItemsDone = i + 1
			state.ItemErrors++
			if state.ItemsDone <= systemicHead && state.ItemErrors == state.ItemsDone {
				state.Status = StatusFailed
				state.Error = fmt.Sprintf("systemic: first %d items all failed, latest: %v", state.ItemsDone, err)
				_ = r.Store.SaveRun(ctx, state)
				return state, fmt.Errorf("evalfcore: systemic failure: %w", err)
			}
			if err := r.Store.SaveRun(ctx, state); err != nil {
				return state, fmt.Errorf("evalfcore: persist after item error %s: %w", item.ID, err)
			}
			continue
		}
		state.Results = append(state.Results, res)
		state.ItemsDone = i + 1
		// 每完成一题落一条 + 更新清单：进度永远不领先于结果
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

	var citedIDs, citedSpans []string
	resolvedCount := 0
	for _, c := range out.Cited {
		citedIDs = append(citedIDs, c.DocID)
		citedSpans = append(citedSpans, c.DocID+"#"+c.Span)
		if c.Resolved {
			resolvedCount++
		}
	}

	res := ItemResult{
		ItemID:            item.ID,
		Refused:           out.Refused,
		RerankApplied:     out.RerankApplied,
		RerankReason:      out.RerankReason,
		CitedDocs:         citedIDs,
		CitedSpans:        citedSpans,
		Answer:            out.Answer,
		RuleScore:         RuleScore(out.Answer, item.Answer),
		EvidenceHit:       EvidenceHit(citedIDs, item.GoldIDs),
		CitationsResolved: resolvedCount,
		CitationsTotal:    len(out.Cited),
		RouteAction:       out.RouteAction,
		Confidence:        out.Confidence,
		RouteTier:         out.RouteTier,
		Coverage:          out.Coverage,
		Margin:            out.Margin,
		Support:           out.Support,
		SupportN:          out.SupportN,
		VerifyNoul:        out.VerifyNoul,
		VerifyChoice:      out.VerifyChoice,
		VerifyConf:        out.VerifyConf,
		DecisionApplied:   out.DecisionApplied,
		DecisionReason:    out.DecisionReason,
		DecisionNoul:      out.DecisionNoul,
		GateBlocked:       out.GateBlocked,
		LatencyMS:         latency,
		PromptTokens:      out.PromptTokens,
		CompletionTokens:  out.CompletionTokens,
		CostKnown:         out.CostKnown,
	}
	if r.Judge != nil && !out.Refused {
		v, jerr := r.Judge.Judge(item.Question, out.Answer, item.Answer)
		if jerr == nil {
			ok := v.OK
			res.JudgeOK = &ok
			res.JudgeCoverage = v.Coverage
			res.JudgeTokens = v.PromptTokens + v.CompletionTokens
			res.JudgeRaw = v.Raw
		} else {
			res.JudgeErr = jerr.Error()
		}
	}
	unresolved := len(out.Cited) - resolvedCount
	// 失败标签只出现在真失败的题上：答错、没命中证据、引用核不掉、拒答，
	// 失败判定四途：规则分不满、证据没命中、引用核不掉、拒答。
	// 判官分是第五途且优先级更高：判官说对（JudgeOK=true）的题不算
	// 失败——规则分是词法基线口径，LLM 答案天然不逐字含金标，拿规则分
	// 判它失败是把口径当事实（真实运行：50 题 LLM 臂规则分 0、判官
	// 46 题可判，全按旧口径会 100% 挂 rot）。判官说不对或未判时，
	// 仍按四途。
	judgeSaysOK := res.JudgeOK != nil && *res.JudgeOK
	failed := !judgeSaysOK && (res.RuleScore < 1 || !res.EvidenceHit || unresolved > 0 || out.Refused)
	if failed {
		res.Failure = FormatCategory(Classify(ClassifyInput{
			Windows:             out.Windows,
			GoldHit:             res.EvidenceHit,
			UnresolvedCitations: unresolved,
			Refused:             out.Refused,
			RouteAction:         out.RouteAction,
			RuleScore:           res.RuleScore,
			AsksNumeric:         failure.AsksNumeric(item.Question),
			EvidenceHasNumeric:  out.EvidenceHasNumeric,
			QueryOutOfCorpus:    out.QueryOutOfCorpus,
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
