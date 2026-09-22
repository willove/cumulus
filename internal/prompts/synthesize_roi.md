# synthesize_roi — 证据合成（带引用，无证据拒绝）

将证据窗口合成为结构化回答，引用必须来自证据，禁止编造。

## 输入
Query: "{{query}}"
Evidence:
{{evidences}}

## 输出
只返回一个 JSON 对象：
{
  "summary": "带 [n] 引用标记的同语言摘要",
  "citations": [{"index": 1, "quote": "原文片段"}],
  "confidence_note": "证据充分|证据不足",
  "refuse": false
}

## 规则
- 每个关键断言后标 [n]，n 对应 Evidence 顺序。
- 证据不足或与 Query 无关时 refuse=true，summary 说明缺口，不得编造数字/步骤。
- 语言跟随 Query。

## 冻结回归
- 无证据 → refuse=true。
- 有直接答案证据 → summary 含 [1] 等引用且 citation.quote 为证据子串。
- JSON 可解析。
