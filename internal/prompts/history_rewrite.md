# history_rewrite — 多轮相关性过滤 + 查询改写

判断对话历史是否与最新用户消息主题相关；若相关，将最新消息改写为不依赖上下文的独立检索查询。消息已自包含则原样返回。

## 输入
Chat History:
{{history}}

Latest User Message: "{{query}}"

## 输出
只返回一个 JSON 对象：
{
  "history_relevant": true | false,
  "standalone_query": "改写后的独立查询或原文",
  "changed": true | false
}

## 规则
- 闲聊/致谢/道别历史不得污染检索改写。
- history_relevant=false 时 standalone_query = 原消息。
- 自包含问句 changed=false。
- 语言跟随 Latest User Message。

## 冻结回归
- 闲聊史 + 检索问 → history_relevant=false，query 不带闲聊词。
- 自包含问句 → changed=false。
- 需指代消解（「它/那个配置」）→ standalone_query 补全指代。
