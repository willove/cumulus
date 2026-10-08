/**
 * cumulus ↔ @wil-works/evoke-chat 的 transport（宿主侧胶水，零依赖）。
 *
 * 放这个文件的原因：Go 里的 internal/harness/evokechat 是**同一套映射的 Go 版**
 * （服务端与测试用）。前端跑不了 Go，所以这里有一份 JS 镜像。两侧必须同步改——
 * 事件词表变了，两边都要动；harness.Kinds() 那条测试守的是服务端那份。
 *
 * 接入方式（三行）：
 *
 *   import { createCumulusTransport } from './cumulus-transport'
 *   const engine = useChatEngine({ onSend: createCumulusTransport({ engine, baseURL: '' }) })
 *
 * onSend 的契约（@wil-works/evoke-chat 的硬规则）：
 *   **必须返回 Promise 直到流结束**——引擎的 loading 挂在这一刻，生成中发送钮才会
 *   变停止钮（stoppable）。提前 resolve 会让 loading 立刻回落、停止钮不出现。
 *   （这条在 examples/ebui-example-ai/src/pages/AiWorkbench.vue 的注释里也写了。）
 *
 * 映射总表（与 Go adapter 一一对应）：
 *
 *   started    → createAssistantMessage
 *   stage      → engine.setProgress(msg.id, { label, detail, elapsedMs })
 *                （组件本来就渲染 message.progress.{label,detail,elapsedMs}，
 *                  契约要求阶段进度走这里、**别拼进 think 文本**；收尾态引擎会清掉）
 *   file       → 同上（检索日志一行）或 工具卡：startToolCall/appendToolCallResult/
 *                completeToolCall（filesAsToolCards: true）
 *   reasoning  → engine.appendThinkContent   ← 思考与正文必须两个 API，混写会串行渲染
 *   content    → engine.appendContent（增量）/ engine.updateMessage({content})（replace）
 *   citations  → onCitations(refs)（**引擎没有引用 setter**：引用面板由宿主落，
 *                通常是 EbChatSources 或 addArtifact）
 *   related    → onRelated（**不要混进 citations**，否则变成"引用了但没引"）
 *   done       → engine.completeMessage + engine.setUsage
 *   error      → engine.setMessageError
 *
 * 下面用到的引擎方法**逐个核过存在性**（v0.4.1 的 useChatEngine 返回值）：
 *   createAssistantMessage / appendContent / appendThinkContent / updateMessage /
 *   completeMessage / setMessageError / cancelMessage / setProgress / setUsage /
 *   startToolCall / appendToolCallResult / completeToolCall / addArtifact。
 *   **没有** setContent / setSources——想整段替换用 updateMessage({ content })。
 *
 * 未知事件：**记录并跳过**，不要静默丢，也不要抛（服务端加新事件不该弄崩旧界面）。
 */

/**
 * @param {object} opts
 * @param {object} opts.engine   useChatEngine() 的返回值
 * @param {string} [opts.baseURL] 后端地址，空 = 同源
 * @param {string} [opts.endpoint] 流式端点（默认 /v1/qa/stream）
 * @param {boolean} [opts.filesAsToolCards] 检索命中走工具调用卡（重但可回看原文）
 * @param {(s: {label:string, detail?:string, percent?:number, ms?:number, kind:string}) => void} [opts.onStage]
 * @param {(refs: Array) => void} [opts.onCitations]
 * @param {(items: Array) => void} [opts.onRelated]
 * @param {(info: object) => void} [opts.onDone]
 * @param {(ev: object) => void} [opts.onFrame] 逐帧观察（调试/自检用）
 * @param {(err: Error) => void} [opts.onError]
 * @returns {(content: string, signal: AbortSignal) => Promise<void>}
 */
export function createCumulusTransport(opts) {
  const {
    engine,
    baseURL = "",
    endpoint = "/v1/qa/stream",
    filesAsToolCards = false,
    onStage, onCitations, onRelated, onDone, onFrame, onError,
  } = opts || {};

  return async function onSend(content, signal) {
    const msg = engine.createAssistantMessage();
    let thinkText = "";
    let answerText = "";
    const toolIds = new Map();   // docId → toolCallId（工具卡模式）
    const unknown = [];          // 没处理的事件（可见，不静默）

    try {
      const resp = await fetch(baseURL + endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question: content, session: msg.id }),
        signal,
      });
      if (!resp.ok) throw new Error("HTTP " + resp.status + " " + (await resp.text()));

      await readSSE(resp, (ev) => {
        if (onFrame) onFrame(ev);
        switch (ev.kind) {
          case "started":
            break;

          case "stage": {
            const s = ev.stage || {};
            // 契约：结构化进度走 progress 事件，**别把阶段进度拼进 think 文本**。
            // setProgress 是引擎真实 API，组件直接渲染 label/detail/elapsedMs；
            // percent（0..100）组件不画（它只有一行读数），要进度条就用 onStage 自绘。
            const stage = {
              label: s.label || s.name,
              detail: s.detail || "",
              elapsedMs: s.duration_ms || 0,
              percent: s.percent || 0,
              name: s.name,
              phase: s.phase,
            };
            engine.setProgress(msg.id, {
              label: stage.label, detail: stage.detail, elapsedMs: stage.elapsedMs,
            });
            if (onStage) onStage(Object.assign({ kind: "stage" }, stage));
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
              }
              if (f.preview) engine.appendToolCallResult(msg.id, id, f.preview);
            } else {
              const hit = {
                label: "命中 " + (f.title || f.doc_id),
                detail: "#" + f.rank + " 得分 " + Number(f.score || 0).toFixed(2),
                elapsedMs: 0, percent: 0,
              };
              engine.setProgress(msg.id, hit);
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
            const refs = (ev.citations || []).map((c) => ({
              sourceId: c.doc_id,
              title: c.title || c.doc_id,
              text: c.text || "",
              url: "/v1/doc/" + encodeURIComponent(c.doc_id),
              resolved: !!c.resolved,
              span: c.span || "",
            }));
            // 引擎**没有**引用 setter：引用面板由宿主落（EbChatSources 插槽 /
            // addArtifact / 自定义卡片）。没给 onCitations 就只留在逐帧记录里。
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
            engine.completeMessage(msg.id);
            engine.setProgress(msg.id, null);   // 收尾清掉进度行（组件的约定）
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

      // 工具卡模式：收尾把每张卡结掉（省略 result = 保留流出的输出）
      if (filesAsToolCards) {
        for (const id of toolIds.values()) engine.completeToolCall(msg.id, id);
      }
      if (unknown.length && onError) {
        onError(new Error("收到未处理事件：" + unknown.join(",")));
      }
      // 流在 done 之前断掉（网络/代理截断）：**如实报错**，不要把半截答案当完成
      if (!answerText && !thinkText) {
        engine.setMessageError(msg.id, "响应流已截断：没有收到任何内容帧");
      }
    } catch (e) {
      if (e && e.name === "AbortError") {
        // 用户主动停止 = cancelled，不是 error（契约：不要用 setMessageError 表达停止）
        engine.cancelMessage(msg.id);
      } else {
        engine.setMessageError(msg.id, String((e && e.message) || e));
        if (onError) onError(e);
      }
    }
  };
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
      const data = frame.split("\n").find((l) => l.startsWith("data: "));
      if (!data) continue;                       // event: 行/注释行/心跳：忽略
      const payload = data.slice(6);
      if (payload === "[DONE]") return;           // 终止哨兵（与 OpenAI 同惯例）
      try { onEvent(JSON.parse(payload)); } catch (err) { console.warn("帧不是 JSON", payload); }
    }
  }
}