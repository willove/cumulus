# evaluate_batch — 批式片段打分（0–10）+ 事实覆盖（oracle 向量）

你是文档检索助手。下面给出同一 Query 的多个文本片段，逐一判断每段是否含有回答问题的线索，并指出它直接支持哪些原子事实。

## 语言约束
检测 Query 语言，reasoning 与 output 使用同一语言（Query 为中文则 reasoning 为中文）。

## 输入
Query: "{{query}}"
Atomic Facts:
{{facts}}
Text Snippets（共 {{count}} 段，编号 [S1]..[S{{count}}]）:
{{windows}}

## 输出要求
只返回一个 JSON 数组（无额外文本），**恰好 {{count}} 个对象，每段一个，id 与输入编号一一对应**：
- score (0-10)（每段独立打分，标尺与单片段评分完全一致）:
  0-3: 完全无关。
  4-7: 含相关关键词或上下文，但无直接答案。
  8-10: 含精确数据、事实或直接答案。
- reasoning: 与 Query 同语言的简短理由。
- covers: 该片段**直接支持**的原子事实 ID 数组（只能取自 Atomic Facts 中给出的 ID；未给出 Atomic Facts 或无对应事实时为空数组）。

示例: [{"id": "S1", "score": 8, "reasoning": "直接给出上限数值。", "covers": ["f1"]}, {"id": "S2", "score": 2, "reasoning": "仅提及主题词。", "covers": []}]

## 冻结回归标尺（勿改语义）
| 样本类型 | 期望区间 |
| --- | --- |
| 无关文本 | 0–3 |
| 仅有关键词 | 4–7 |
| 直接答案/精确数字 | 8–10 |
| covers | ⊆ Atomic Facts 给定 ID；无 Atomic Facts 时必为 [] |
| 数组长度 | 恰好等于输入片段数，id 一一对应 |
