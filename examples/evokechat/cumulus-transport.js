/**
 * cumulus ↔ @wil-works/evoke-chat 的 transport（宿主侧胶水，零依赖）。
 *
 * 放这个文件的原因：Go 里的 internal/harness/evokechat 是**同一套映射的 Go 版**
 * （服务端与测试用）。前端跑不了 Go，所以这里是一份 JS 镜像。两侧必须同步改——
 * 事件词表变了，两边都要动；harness.Kinds() 那条测试守的是服务端那份。
 *
 * 接入方式（三行）：
 *
 *   import { createCumulusTransport } from './cumulus-transport'
 *   const engine = useChatEngine({ onSend: createCumulusTransport({ engine }) })
 *
 * onSend 的契约（@wil-works/evoke-chat 的硬规则）：
 *   **必须返回 Promise 直到流结束**——引擎的 loading 挂到这一刻，生成中发送钮才会
 *   变停止钮（stoppable）。提前 resolve 会让 loading 立刻回落，停止钮不出现。
 *
 * ── 对 0.5.0 新 API 的用法（本次核对过的）────────────────────────────
 *
 *   阶段进度 → engine.appendProgress(msg.id, {label, detail, elapsedMs, percent})
 *              内部流水线时间线（progressLog），收尾 engine.clearProgress。
 *              0.4.x 没有它 → 自动退回 setProgress（单行当前态），行为不丢。
 *   行内引用 → anchorSource(refId, text) 生成 `source:` 锚点，配合 EbChatSources
 *              高亮（refId 必须与 refs[].sourceId 一一对应）。
 *   流式入参 → engine.appendToolCallArgs(msg.id, callId, chunk)
 *              0.4.x 没有它 → 入参一次给全量（仍然能跑，只是看不到参数在流）。
 *   结构化结果 → engine.completeToolCall(msg.id, callId, result, {resultType})
 *
 * ── 映射总表 ────────────────────────────────────────────────────────
 *
 *   started    → createAssistantMessage
 *   stage      → appendProgress（阶段时间线）
 *   file       → appendProgress（检索日志一行）/ 工具卡（filesAsToolCards）
 *   reasoning  → appendThinkContent   ← 思考与正文必须两个 API，混写会串行渲染
 *   content    → appendContent（增量）/ updateMessage({content})（replace 整段替换）
 *   citations  → onCitations(refs) + 行内锚点脚注（anchorFooter: true）
 *   related    → onRelated（**不要混进 citations**，否则变成"引用了但没引"）
 *   done       → completeMessage + clearProgress + setUsage
 *   error      → setMessageError
 *
 * 下面用到的引擎方法**逐个核过存在性**（v0.5.0 useChatEngine 返回值）：无虚构调用。
 * 引擎**没有** setContent / setSources——整段替换用 updateMessage({content}），
 * 引用面板由宿主落（EbChatSources 插槽 / addArtifact / onCitations）。
 *
 * 未知事件：**记录并跳过**，不静默丢、不抛（服务端加新事件不该弄崩旧界面）。
 */

/**
 * @param {object} opts
 * @param {object} opts.engine   useChatEngine() 的返回值
 * @param {string} [opts.baseURL] 后端地址，空 = 同源
 * @param {string} [opts.endpoint] 流式端点（默认 /v1/qa/stream）
 * @param {boolean} [opts.filesAsToolCards] 检索命中走工具调用卡（重但可回看原文）
 * @param {boolean} [opts.anchorFooter] 收尾时给答案补一行行内引用锚点（默认 true）
 * @param {(entry: object) => void} [opts.onStage] 自绘进度条时用（percent 组件不画）
 * @param {(refs: Array) => void} [opts.onCitations]
 * @param {(items: Array) => void} [opts.onRelated]
 * @param {(info: object) => void} [opts.onDone]
 * @param {(ev: object) => void} [opts.onFrame] 逐帧观察（调试/自检用）
 * @param {(err: Error) => void} [opts.onError]
 */
export function createCumulusTransport(opts) {
  const {
    engine,
    baseURL = "",
    endpoint = "/v1/qa/stream",
    filesAsToolCards = false,
    anchorFooter = true,
    onStage, onCitations, onRelated, onDone, onFrame, onError,
  } = opts || {};

  // 能力探测：**缺席是合法状态**（0.4.x 没有新 API 也要能跑，退回旧通道）。
  const has = (name) => typeof engine[name] === "function";
  const canProgressLog = has("appendProgress") && has("clearProgress");
  const canStreamArgs = has("appendToolCallArgs");
  const anchorSource = anchorSourceImpl;

  // 兼容两种调用形态：
  //   直连 useChatEngine({ onSend }) → (content, signal)
  //   经 EbAiConsole 的 transport prop → (content, attachments, context)
  // 第二个参数**可能是 AbortSignal，也可能是附件数组**——按形状判断，不靠约定。
  let currentAbort = null;

  async function onSend(content, arg2, arg3) {
    const signal = (arg2 && typeof arg2.aborted === "boolean") ? arg2
      : (arg3 && typeof arg3.aborted === "boolean") ? arg3 : null;
    currentAbort = new AbortController();
    const msg = engine.createAssistantMessage();
    let thinkText = "";
    let answerText = "";
    const refs = [];
    const toolIds = new Map();   // docId → toolCallId（工具卡模式）
    const unknown = [];          // 没处理的事件（可见，不静默）

    try {
      const resp = await fetch(baseURL + endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question: content, session: msg.id }),
        signal: currentAbort.signal,
      });
      if (!resp.ok) throw new Error("HTTP " + resp.status + " " + (await resp.text()));

      await readSSE(resp, (ev) => {
        if (onFrame) onFrame(ev);
        switch (ev.kind) {
          case "started":
            break;

          case "stage": {
            const s = ev.stage || {};
            const entry = {
              label: s.label || s.name,
              detail: s.detail || "",
              elapsedMs: s.duration_ms || 0,
              percent: s.percent || 0,
              name: s.name,
              phase: s.phase,
            };
            // 0.5.0：进时间线；0.4.x：退回单行当前态（行为不丢，只是没有历史）
            if (canProgressLog) engine.appendProgress(msg.id, entry);
            else engine.setProgress(msg.id, { label: entry.label, detail: entry.detail, elapsedMs: entry.elapsedMs });
            if (onStage) onStage(Object.assign({ kind: "stage" }, entry));
            break;
          }

          case "file": {
            const f = ev.file || {};
            if (filesAsToolCards) {
              let id = toolIds.get(f.doc_id);
              if (!id) {
                id = engine.startToolCall(msg.id, {
                  name: "knowledge_retrieval",
                  label: "检索证据",
                  args: { rank: f.rank, docId: f.doc_id, score: f.score, span: f.span },
                });
                toolIds.set(f.doc_id, id);
                // 长参数可以在流里逐片拼（0.5.0）：这里把 docId/span 分两片演示，
                // 0.4.x 没有这个 API → 跳过（入参已在 startToolCall 给全量）。
                if (canStreamArgs && f.span) {
                  engine.appendToolCallArgs(msg.id, id, '{"docId":"' + f.doc_id + '","span":');
                  engine.appendToolCallArgs(msg.id, id, JSON.stringify(f.span) + "}");
                }
              }
              if (f.preview) engine.appendToolCallResult(msg.id, id, f.preview);
            } else {
              const hit = {
                label: "命中 " + (f.title || f.doc_id),
                detail: "#" + f.rank + " 得分 " + Number(f.score || 0).toFixed(2),
                elapsedMs: 0, percent: 0,
              };
              if (canProgressLog) engine.appendProgress(msg.id, hit);
              else engine.setProgress(msg.id, hit);
              if (onStage) onStage(Object.assign({ kind: "hit" }, hit));
            }
            break;
          }

          case "reasoning": {
            // 思考与正文**必须两个 API**（契约硬规则）：混写会串行渲染。
            const t = (ev.reasoning || {}).text || "";
            thinkText += t;
            engine.appendThinkContent(msg.id, t);
            break;
          }

          case "content": {
            const c = ev.content || {};
            if (c.replace) {
              // 整段替换：引擎**没有** setContent，用 updateMessage({content})
              answerText = c.text || "";
              engine.updateMessage(msg.id, { content: answerText });
            } else {
              answerText += c.text || "";
              engine.appendContent(msg.id, c.text || "");
            }
            break;
          }

          case "citations": {
            for (const c of ev.citations || []) {
              refs.push({
                id: c.doc_id,
                sourceId: c.doc_id,
                title: c.title || c.doc_id,
                text: c.text || "",
                url: "/v1/doc/" + encodeURIComponent(c.doc_id),
                resolved: !!c.resolved,
                span: c.span || "",
              });
            }
            if (onCitations) onCitations(refs);
            break;
          }

          case "related": {
            // 关联文档**独立**于引用面板：混进去就变成"引用了但没引"。
            if (onRelated) {
              onRelated((ev.related || []).map((r) => ({
                sourceId: r.doc_id,
                title: r.title || r.doc_id,
                why: r.why || "related",
                preview: r.preview || "",
                url: "/v1/doc/" + encodeURIComponent(r.doc_id),
              })));
            }
            break;
          }

          case "error":
            engine.setMessageError(msg.id, ev.error || "流式检索失败");
            break;

          case "done": {
            const d = ev.done || {};
            // 行内引用脚注：答案正文补一行"根据 [1](source:doc-1)…"，
            // 点上标联动 EbChatSources 高亮（anchorSource 的 refId 要与
            // citations[].id 一一对应——这里用的就是 doc_id）。
            let content = answerText;
            if (anchorFooter && refs.length && anchorSource) {
              content += "\n\n根据 " +
                refs.map((r, i) => anchorSource(r.id, i + 1)).join("、") +
                " 篇文档。";
            }
            if (content !== answerText) {
              engine.updateMessage(msg.id, { content });
              answerText = content;
            }
            engine.completeMessage(msg.id);
            if (canProgressLog) engine.clearProgress(msg.id);
            else engine.setProgress(msg.id, null);
            engine.setUsage(msg.id, {
              promptTokens: d.prompt_tokens || 0,
              completionTokens: d.completion_tokens || 0,
              ttftMs: d.latency_ms || 0,
            });
            if (onDone) onDone(d);
            break;
          }

          default:
            // 未知事件：记下来，不静默丢、不抛（服务端加事件不该弄崩旧界面）
            unknown.push(ev.kind || "?");
        }
      });

      // 工具卡收尾：结掉每张卡（省略 result = 保留流出的输出），并声明结果形态
      if (filesAsToolCards) {
        for (const id of toolIds.values()) {
          engine.completeToolCall(msg.id, id, undefined, { resultType: "text" });
        }
      }
      if (unknown.length && onError) {
        onError(new Error("收到未处理事件：" + unknown.join(",")));
      }
      // 流在 done 之前断掉（网络/代理截断）：**如实报错**，不要把半截答案当完成
      if (!answerText && !thinkText) {
        engine.setMessageError(msg.id, "响应流已截断：没有收到任何内容帧");
      }
    } catch (e) {
      const aborted = (e && e.name === "AbortError") || signal?.aborted || currentAbort?.signal.aborted;
      if (aborted) {
        // 用户主动停止 = cancelled，不是 error（契约：不要用 setMessageError 表达停止）
        engine.cancelMessage(msg.id);
      } else {
        engine.setMessageError(msg.id, String((e && e.message) || e));
        if (onError) onError(e);
      }
    } finally {
      currentAbort = null;
    }
  }

  /** 宿主停止按钮调用（EbAiConsole 的 @stop）。中止当前那一轮。 */
  onSend.cancel = () => {
    if (currentAbort) currentAbort.abort();
  };
  onSend.isStreaming = () => !!currentAbort;
  return onSend;
}

/**
 * 行内引用锚点：`[序号](source:<refId>)`，正文渲染成可点上标，点击联动
 * EbChatSources 高亮对应卡片。
 *
 * 为什么内联一份而不是 import：`@wil-works/evoke-chat` 的 utils 不是公开入口
 * （exports 里只有组件与 composables），而这份只依赖两个字符串规则——
 * **等它进公开 API 后，换成 import 即可，行为一致**。
 */
function anchorSourceImpl(refId, text) {
  const id = String(refId ?? "").replace(/[\s()]/g, "");
  return `[${text ?? ""}](source:${id})`;
}

/**
 * SSE 解析（fetch + ReadableStream）。
 * 为什么不用 EventSource：它只能 GET，不能 POST 也不带自定义头。
 * 为什么自己按 \n\n 切：SSE 允许注释行与多行 data；切错就会把半帧当整帧。
 */
async function readSSE(resp, onEvent) {
  const reader = resp.body.getReader();
  const dec = new TextDecoder();
  let buf = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true });
    let i;
    while ((i = buf.indexOf("\n\n")) >= 0) {
      const frame = buf.slice(0, i);
      buf = buf.slice(i + 2);
      const line = frame.split("\n").find((l) => l.startsWith("data: "));
      if (!line) continue;                       // event: 行/注释行/心跳：忽略
      const payload = line.slice(6);
      if (payload === "[DONE]") return;           // 终止哨兵（与 OpenAI 同惯例）
      try { onEvent(JSON.parse(payload)); } catch (err) { console.warn("帧不是 JSON", payload); }
    }
  }
}