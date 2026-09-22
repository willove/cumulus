# fast_analyze — FAST 档查询分析（意图 + 两级关键词 + IDF）

对用户查询分类；若为文档/检索类查询，抽取两级粒度检索词。

## 输入
User Query: "{{query}}"

## 输出
只返回一个 JSON 对象：
{
  "intent": "search" | "chat" | "doc_summary",
  "primary": {"词或短语": idf_weight, ...},
  "fallback": {"更细粒度词": idf_weight, ...},
  "keywords_alt": {"同义/英文对照": idf_weight, ...}
}

## 规则
- intent=chat：问候、身份、闲聊、致谢、道别。
- intent=doc_summary：整文档操作（总结/翻译/通读分析），不是找一段证据。
- intent=search：其余信息检索问题。
- primary 2–5 个高特异性词；fallback 更细（级联 miss 后降级）。
- idf_weight ∈ (0,1]，越独特越高。
- 语言跟随 Query。

## 冻结回归
- 三类意图不得串档。
- search 意图下 primary 非空。
- JSON 可解析，无额外文本。
