package main

import (
	"bufio"
	gocontext "context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/decide"
	"github.com/willove/cumulus/internal/deepcore"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/evaldata"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/minilm"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/rerank"
	"github.com/willove/cumulus/internal/synth"
)

// llmCompleter 是 llm.Completer 在本包的别名（少写一层包名）。
type llmCompleter = llm.Completer

func llmFromEnvImpl() (*llm.OpenAICompleter, error) {
	return llm.FromEnv(os.Getenv("LLM_BASE_URL"), os.Getenv("LLM_API_KEY"), os.Getenv("LLM_CHAT_MODEL"))
}

// loadDotEnv 把 .env 里的 KEY=VALUE 补进进程环境（不覆盖已设置的变量）。
// 只认最简单的形状：无引号、无导出、无注释续行—— cumulus-next 的配置
// 就是.env 一把钥匙，复杂配置留给配置面（还没建）。
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return // 没有 .env 就用进程环境，不报错
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

// pickSynth 按选择装配合成面：offline（默认，无网络）或 llm（真提供方）。
// llm 缺配置时直接报错——不许静默回落到桩。
// pickSynth 装合成面，并**顺带声明它有没有流式能力**。
//
// 为什么第二个返回值不能省：流式能力必须**在接线处显式声明**——把
// `l.Synthesize`（方法值）传进 Options.Synth 之后，`SynthesizeStream` 这个方法
// 在类型层面就消失了，靠类型断言找不回来（真跑踩过：端点正常、事件正常，
// 但正文仍是整段）。所以这里同时把流式那条腿交出来，nil = 只支持整条。
func pickSynth(which string) (qaflow.SynthFunc, qaflow.StreamSynthFunc, string, error) {
	switch which {
	case "", "offline":
		return func(q string, ws []qaflow.EvidenceWindow, fx facts.Report) (qaflow.Answer, qaflow.Usage, error) {
			return synth.Offline(q, ws, fx)
		}, nil, "offline", nil
	case "llm":
		c, err := llmFromEnvImpl()
		if err != nil {
			return nil, nil, "", err
		}
		l := &synth.LLM{Client: c}
		label := "llm:" + c.Model + "@" + hostOf(c.BaseURL)
		// 上游补全器不支持流式（llm.Streamer 没实现）时也不接这条腿——
		// 免得"接了但每步都退回调"，读数上却显示流式开着。
		if !llm.SupportsStream(c) {
			return l.Synthesize, nil, label + " (no-stream)", nil
		}
		return l.Synthesize, adaptSynthStream(l), label, nil
	default:
		return nil, nil, "", errUnknownSynth(which)
	}
}

// adaptSynthStream 把 synth 的 PieceFunc 形状翻译成 qaflow 的（apps/cmd 这条
// 边是合法的：pipeline 不反向依赖 capabilities，所以形状在两侧各声明一次）。
func adaptSynthStream(l *synth.LLM) qaflow.StreamSynthFunc {
	return func(q string, ws []qaflow.EvidenceWindow, fx facts.Report, fn qaflow.PieceFunc) (qaflow.Answer, qaflow.Usage, error) {
		return l.SynthesizeStream(q, ws, fx, func(p synth.StreamPiece) error {
			if fn == nil {
				return nil
			}
			return fn(qaflow.Piece{Reasoning: p.Reasoning, Content: p.Content})
		})
	}
}

type errUnknownSynth string

func (e errUnknownSynth) Error() string { return "unknown synth: " + string(e) + " (offline|llm)" }

func hostOf(baseURL string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://")
	if i := strings.IndexByte(s, '/'); i > 0 {
		s = s[:i]
	}
	return s
}

// llmFromEnv 从环境变量装配真提供方（.env 已由 loadDotEnv 补入）。
func llmFromEnv() (llmCompleter, error) {
	return llmFromEnvImpl()
}

// pickEmbed 装向量面：minilm（本地权重，缺权重报错——不许静默无向量）或
// off（默认，不绑 embedder）。
func pickEmbed(which string) (func() embed.Embedder, string, error) {
	switch which {
	case "", "off":
		return nil, "off", nil
	case "minilm":
		m := embed.NewMiniLM(minilm.DefaultDir())
		if !m.Available() {
			return nil, "", fmt.Errorf("embed: weights absent at %s — run `cumulus model install`", minilm.DefaultDir())
		}
		return func() embed.Embedder { return m }, "minilm@" + minilm.DefaultDir(), nil
	default:
		return nil, "", fmt.Errorf("unknown embed: %s (off|minilm)", which)
	}
}

// selectorFromEnv 按 CUMULUS_RERANK 装窗口选择器。
//
//	CUMULUS_RERANK=off（默认）  覆盖贪心（零成本，可离线复现）
//	CUMULUS_RERANK=llm         清单式交叉编码器（一次调用处理整池）
//
// 它只在**池子大于预算**时才有意义：单轮 k9 的池子就是 9 条预算 9 条，
// 选择器无事可做（rerank.Select 会直接返回，不花钱）。所以想验证选择器
// 就要跑深循环臂（池 27 → 留 9）。
func selectorFromEnv() deepcore.Selector {
	switch os.Getenv("CUMULUS_RERANK") {
	case "llm":
		c, err := llmFromEnvImpl()
		if err != nil {
			// 装配失败要早报：静默退回覆盖贪心会让"重排没生效"变成
			// 一个测不出来的读数（真跑踩过这类哑退化）。
			fmt.Printf("selector: CUMULUS_RERANK=llm 但提供方不可用：%v（退回覆盖贪心，重排不发生）\n", err)
			return nil
		}
		r := &rerank.LLM{Client: c}
		return func(ctx gocontext.Context, query string, pool []deepcore.Window, budget int) ([]deepcore.Window, error) {
			sel, err := r.Select(ctx, query, pool, budget)
			if r.Reason != "" {
				fmt.Printf("  rerank: %s（池 %d → 选 %d）\n", r.Reason, len(pool), len(sel))
			}
			return sel, err
		}
	default:
		return nil
	}
}

// decideFromEnv 装决策模型客户端（DASHSCOPE_API_KEY）。密钥只从环境读，
// .env 已 gitignore——**任何情况下不把密钥写进仓库**。
func decideFromEnv() (*decide.Client, error) {
	if vc == nil {
		var err error
		vc, err = decide.FromEnv()
		if err != nil {
			return nil, err
		}
	}
	return vc, nil
}

// vc 是决策模型客户端的单例（建一次、复用连接；客户端本身并发安全）。
var vc *decide.Client

// coordFromEnv 返回检索协调因子指数（0 = 关，默认）。
// CUMULUS_COORD=1 启用 (命中词数/查询词数)^lambda：多实体问句里，"覆盖了
// 问句几个实体"比"某个实体词反复命中"更该排前面（DomainRAG multidoc 实测：
// 协调前金标 ≤3 名只有 8/48）。
func coordFromEnv() float64 {
	v := os.Getenv("CUMULUS_COORD")
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// deepFromEnv 从默认档出发按环境微调深循环（实验旋钮：池子大小与预算
// 决定"选择阶段有没有余量"）。
//
//	CUMULUS_DEEP_BUDGET  最终保留窗口数（默认 9，与 k9 同预算）
//	CUMULUS_DEEP_PAGE    每轮新候选数（默认 9）
//	CUMULUS_DEEP_ROUNDS  轮数（默认 3，池子 = PAGE×ROUNDS）
func deepFromEnv() qaflow.DeepOptions {
	d := qaflow.DefaultDeep()
	d.Selector = selectorFromEnv()
	if v, ok := envInt("CUMULUS_DEEP_BUDGET"); ok {
		d.Budget = v
	}
	if v, ok := envInt("CUMULUS_DEEP_PAGE"); ok {
		d.PageSize = v
	}
	if v, ok := envInt("CUMULUS_DEEP_ROUNDS"); ok {
		d.MaxRounds = v
	}
	return d
}

func envInt(name string) (int, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// applySplitAndSample 按环境先切分再采样。顺序是刻意的——**切分决定
// "这题属于哪一份"（身份，稳定，与顺序无关），采样只是"这一份里跑多少题"
// 的成本旋钮**。CUMULUS_SPLIT=calib|val|lockbox（题 id 哈希，默认 50/25/25），
// CUMULUS_SAMPLE=N 对切分后的集合生效。
func applySplitAndSample(items []evalfcore.Item) ([]evalfcore.Item, error) {
	// 校准切分：CUMULUS_SPLIT=calib|val|lockbox 按题 id 哈希取一份。顺序是
	// 刻意的——**先切分再采样**：切分决定"这题属于哪一份"（身份，稳定，
	// 与顺序无关），采样只是"这一份里跑多少题"的成本旋钮。
	if name := os.Getenv("CUMULUS_SPLIT"); name != "" {
		calibFrac, valFrac := evaldata.DefaultSplit()
		if v := os.Getenv("CUMULUS_SPLIT_CALIB"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f < 1 {
				calibFrac = f
			}
		}
		if v := os.Getenv("CUMULUS_SPLIT_VAL"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 && f < 1 {
				valFrac = f
			}
		}
		parts := evaldata.SplitItems(items, calibFrac, valFrac)
		picked, ok := parts[evaldata.SplitName(name)]
		if !ok {
			return nil, fmt.Errorf("unknown CUMULUS_SPLIT %q (calib|val|lockbox)", name)
		}
		fmt.Printf("split %s: %d/%d 题（calib=%.0f%% val=%.0f%% 余下锁箱；题 id 哈希，与顺序无关）\n",
			name, len(picked), len(items), calibFrac*100, valFrac*100)
		items = picked
	}
	// 采样：把题数压到 N（成本旋钮；0 = 全量）。对**切分后的集合**生效
	// ——"跑 300 题"指的是这一份里的 300 题，不是全库前 300 题。
	if v := os.Getenv("CUMULUS_SAMPLE"); v != "" {
		n := 0
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return nil, fmt.Errorf("CUMULUS_SAMPLE not a number: %q", v)
		}
		if n > 0 && n < len(items) {
			items = items[:n]
		}
	}
	return items, nil
}
