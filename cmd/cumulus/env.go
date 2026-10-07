package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/judge"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/minilm"
	"github.com/willove/cumulus/internal/qaflow"
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

// deepFromEnv 从默认档出发按环境微调深循环（实验旋钮：池子大小与预算
// 决定"选择阶段有没有余量"）。
//
//	CUMULUS_DEEP_BUDGET  最终保留窗口数（默认 9，与 k9 同预算）
//	CUMULUS_DEEP_PAGE    每轮新候选数（默认 9）
//	CUMULUS_DEEP_ROUNDS  轮数（默认 3，池子 = PAGE×ROUNDS）
func deepFromEnv() qaflow.DeepOptions {
	d := qaflow.DefaultDeep()
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
