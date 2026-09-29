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
- **用户术语设定必须并入**：历史中若出现用户对术语的设定、更正或别名说明（「X 就是 Y」「X 又叫 Y」「X 按 Y 理解」「补充设定：X 指 Y」），且最新消息使用了该术语，则 standalone_query 须把规范名并进术语处（如 女儿国 → 女儿国（西梁女国）），changed=true。这是用户显式给出的检索指引，不是闲聊上下文；只认用户原话写明的等同关系，不得自行补充历史中未出现的任何同义词。

## 冻结回归
- 闲聊史 + 检索问 → history_relevant=false，query 不带闲聊词。
- 自包含问句（无相关历史）→ changed=false。
- 需指代消解（「它/那个配置」）→ standalone_query 补全指代。
- 用户设定「X 就是 Y」+ 含 X 的自包含问句 → standalone_query 含 Y，changed=true。
