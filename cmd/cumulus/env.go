package main

import (
	"bufio"
	gocontext "context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/deepcore"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/evaldata"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/judge"
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

// judgeFromEnv 装配判官：CUMULUS_JUDGE=llm 时用真提供方，否则 nil（N/A）。
func judgeFromEnv(which string) (judge.Judge, error) {
	if which != "llm" {
		return nil, nil
	}
	c, err := llmFromEnvImpl()
	if err != nil {
		return nil, err
	}
	return &judge.LLM{Client: c}, nil
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
func pickSynth(which string) (qaflow.SynthFunc, string, error) {
	switch which {
	case "", "offline":
		return func(q string, ws []qaflow.EvidenceWindow, fx facts.Report) (qaflow.Answer, qaflow.Usage, error) {
			return synth.Offline(q, ws, fx)
		}, "offline", nil
	case "llm":
		c, err := llmFromEnvImpl()
		if err != nil {
			return nil, "", err
		}
		l := &synth.LLM{Client: c}
		label := "llm:" + c.Model + "@" + hostOf(c.BaseURL)
		return l.Synthesize, label, nil
	default:
		return nil, "", errUnknownSynth(which)
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
