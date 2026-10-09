package api

// doc.go —— 知识文档生成端点（POST /v1/docs）。
//
// 它是**语料的生产入口**：把检索到的证据整理成一篇可核对的文档写回语料，
// 于是**下一轮问答能引用它**（整理一次、问答受益）。问答回答"这一次问的"，
// 文档沉淀"这一整块知识"——后者才是个人知识库的主要产物。
//
// 三条纪律（与 docgen 包一致，端点这边只负责"别把事情做糊"）：
//   - **文档写进调用者的 realm**：多租户边界不能因为"生成"而绕过；
//   - **生成失败不给半截**：失败就是 5xx + 原因，不返回"生成了一部分"；
//   - **生成完索引要失效**：新文档必须能被下一轮检索到（否则"写进去了却查不到"）。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	gocontext "context"

	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/docgen"
)

// DocGen 是生成能力（可选件）：服务端没装 LLM 时为 nil，端点回 501。
type DocGen = docgen.Generator

type generateDocReq struct {
	Topic string `json:"topic"`
	TopK  int    `json:"top_k,omitempty"`
	Width int    `json:"width,omitempty"`
	// Store 为 false 时只生成不落库（预览/审阅用）。
	Store bool `json:"store,omitempty"`
}

// GenerateDocResponse 是生成结果。Body 是**进语料的正文**（人读 + 可检索）。
type GenerateDocResponse struct {
	ID       string           `json:"id,omitempty"` // 落库后的文档 id（store=false 时为空）
	Topic    string           `json:"topic"`
	Title    string           `json:"title"`
	Body     string           `json:"body"`
	Sections []docgen.Section `json:"sections"`
	Gaps     []docgen.Gap     `json:"gaps,omitempty"`
	Coverage docgen.Coverage  `json:"coverage"`
	Stored   bool             `json:"stored"`
	// Version / Updated / Note 是**同主题文档的增量**：再次生成同一主题是**更新
	// 那一篇**（同 id 覆盖），不是再堆一篇内容重叠的文档。
	Version int    `json:"version"`
	Updated bool   `json:"updated"`
	Note    string `json:"note,omitempty"`
	// IndexRebuilt 是本次写入后是否重建了索引（下一轮检索才看得到）。
	IndexRebuilt bool `json:"index_rebuilt"`
}

func (s *Server) handleGenerateDoc(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.DocGen == nil || s.DocGen.Client == nil {
		writeErr(w, http.StatusNotImplemented, "docgen not configured（需要 LLM）")
		return
	}
	if s.Store == nil {
		writeErr(w, http.StatusNotImplemented, "no store wired（无法落库）")
		return
	}
	var req generateDocReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if len(req.Topic) == 0 {
		writeErr(w, http.StatusBadRequest, "topic required")
		return
	}

	realm := s.realmOf(r)
	ctx := gocontext.Background()
	idx, err := s.IndexFor(r.Context(), realm)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "index: "+err.Error())
		return
	}
	topK, width := req.TopK, req.Width
	if topK <= 0 {
		topK = s.TopK
	}
	if topK <= 0 {
		topK = 9
	}
	if width <= 0 {
		width = s.Width
	}
	if width <= 0 {
		width = 400
	}
	// 检索用的是**本 realm 的索引**：生成文档不能看见别的租户的内容。
	//
	// boost：生成文档下沉一档（实测依据见 docgen.BoostGenerated）——它们是源文档
	// 的转述，不压就永远排在源后面，"整理一次、问答受益"会落空。
	hits := idx.SearchWith(req.Topic, topK, width, func(docID string) float64 {
		if strings.Contains(docID, genPrefix) {
			return docgen.BoostGenerated
		}
		return 1
	})
	doc, err := s.DocGen.Generate(gocontext.Background(), req.Topic, hits)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "generate: "+err.Error())
		return
	}

	out := GenerateDocResponse{
		Topic: doc.Topic, Title: doc.Title, Body: doc.Body,
		Sections: doc.Sections, Gaps: doc.Gaps, Coverage: doc.Coverage,
	}
	if req.Store {
		// 同主题 → 同 id（覆盖写）：先读旧版，算出这一版的增量说明。
		// 不这么做的话，语料里会堆出好几篇内容高度重叠的同主题文档（检索互相
		// 竞争、读者分不清哪篇最新）——"整理一次、问答受益"就变成"整理越多越难查"。
		topicKey := docgen.TopicKey(req.Topic)
		id := genPrefix + topicKey
		prev := s.prevDoc(ctx, realm, id)
		// 语义判据：词面判据抓不住"请求减缓" vs "申请缓缴"这类同义改写（真跑量到）。
		// 判据缺席时 ReviseWith 自动退化为词面口径（保守：多报差异而非谎报合并）。
		rev := docgen.ReviseWith(prev, doc, corpusSources(prev), docgen.SourcesOf(doc), docgenJudge(s.DocGen))
		doc.Version, doc.Note = rev.Version, rev.Note()
		doc.Body = docgen.Stamp(docgen.ApplyMarker(doc.Body), rev)
		out.Version, out.Note = rev.Version, rev.Note()
		if perr := s.putDoc(ctx, realm, id, topicKey, doc.Body, rev, doc.Sources); perr != nil {
			writeErr(w, http.StatusInternalServerError, "store: "+perr.Error())
			return
		}
		out.ID = id
		out.Stored = true
		out.Updated = prev != nil
		// 写完要让本 realm 的索引失效：否则"生成成功了、下一轮却查不到"。
		s.InvalidateRealm(realm)
		if _, ierr := s.IndexFor(r.Context(), realm); ierr == nil {
			out.IndexRebuilt = true
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if false {
	}
	writeJSON(w, http.StatusOK, out)
}

// genPrefix 是生成文档 id 的前缀（内容哈希前 6 位 + "-"）。
//
// **为什么用 id 前缀而不是查正文**：正文里的 marker 是给"识别用途"用的，
// 而检索期每次查询都去扫每篇正文会拖慢每次检索——前缀是 O(1) 的。
// 代价是**前缀可能被伪造**：别的文档恰好以它开头会被误判。概率极低，但一旦发生
// 的后果只是"这篇被降权一档"，不是正确性问题——所以这个取舍是安全的。
const genPrefix = "gen-"

// putDoc 写一篇文档到**调用者的 realm 集合**（生成文档与手抄文档同源同口径）。
func (s *Server) putDoc(ctx gocontext.Context, realm, id, topicKey, body string, rev docgen.Revision, sources []string) error {
	coll := corpus.CollectionFor(realm)
	if err := s.Store.EnsureCollection(ctx, coll); err != nil {
		return err
	}
	return s.Store.PutStruct(ctx, coll, id, corpus.Doc{
		ID: id, Body: body, Encoding: "text/plain; charset=utf-8",
		Kind: "generated", TopicKey: topicKey,
		Version: rev.Version, Sources: sources, GeneratedAt: time.Now(),
	})
}

// prevDoc 读同主题的上一版（不存在 → nil，不算错误：首次生成就是没有上一版）。
func (s *Server) prevDoc(ctx gocontext.Context, realm, id string) *docgen.Document {
	var d corpus.Doc
	if err := s.Store.GetStruct(ctx, corpus.CollectionFor(realm), id, &d); err != nil {
		return nil
	}
	return docFromBody(d)
}

// docFromBody 从正文里还原结构化条目（只为算增量：上一版只需要"有哪些论断"）。
//
// 为什么不另存结构化副本：多一份真相就多一处会漂移的地方。正文里
// "- 论断 [source]" 的形状是固定的，反解即可；解不出就退化为"无可比对内容"
// （Note 变成"内容与上一版一致"——诚实的降级，不是静默错误）。
func docFromBody(d corpus.Doc) *docgen.Document {
	out := &docgen.Document{ID: d.ID, Body: d.Body, Version: d.Version, Sources: d.Sources}
	var heading string
	for _, line := range strings.Split(d.Body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "<!--"):
			// 版本标记行：跳过（内容比较不看它）
		case strings.HasPrefix(t, "- "):
			text, src := splitClaim(strings.TrimPrefix(t, "- "))
			out.Sections = append(out.Sections, docgen.Section{Heading: heading,
				Claims: []docgen.Claim{{Text: text, SourceID: src}}})
		case strings.HasPrefix(t, "#"):
			heading = strings.TrimSpace(strings.TrimLeft(t, "# "))
		}
	}
	return out
}

// splitClaim 拆 "- 论断 [source]"。
func splitClaim(s string) (text, source string) {
	i := strings.LastIndex(s, " [")
	if i < 0 || !strings.HasSuffix(s, "]") {
		return s, ""
	}
	return s[:i], s[i+2 : len(s)-1]
}

// corpusSources 是上一版的源清单（旧文档可能没这份元数据 → 空）。
func corpusSources(d *docgen.Document) []string {
	if d == nil {
		return nil
	}
	return d.Sources
}

// docgenJudge 从生成器借一个判据客户端（判据是**同一模型**的一次额外调用；
// 没有客户端就返回 nil = 纯词面口径）。
func docgenJudge(g *DocGen) docgen.EquivalentJudge {
	if g == nil || g.Client == nil {
		return nil
	}
	j := &docgen.EquivalentJudgeLLM{Client: g.Client}
	return j.Judge()
}

// handleSuggestTopics 是**选题建议**（GET /v1/docs/topics）。
//
// 它**只给候选，不生成**：自动往语料里灌文档等于用没验证的规则污染知识库。
// 人挑一个主题，走 POST /v1/docs 生成——这条人工确认的闸门是刻意的。
//
// 候选来自使用信号的"不满意"族（拒答后追问 / 答后追问）：那些正是知识库的空缺处。
// cite（引用被点开）说明那段知识**已经够用**，不作为选题。
func (s *Server) handleSuggestTopics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.Signals == nil {
		writeErr(w, http.StatusNotImplemented, "no signal store（没有使用信号就没有建议）")
		return
	}
	if s.Store == nil {
		writeErr(w, http.StatusNotImplemented, "no store wired")
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	realm := s.realmOf(r)
	covers := func(topicKey string) (string, bool) {
		id := genPrefix + topicKey
		var d corpus.Doc
		if err := s.Store.GetStruct(gocontext.Background(), corpus.CollectionFor(realm), id, &d); err != nil {
			return "", false
		}
		return id, true
	}
	topics, err := docgen.SuggestTopics(s.Signals, limit, covers)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "suggest: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"topics": topics,
		"note":   "只建议不生成：挑一个主题走 POST /v1/docs（covered=true 表示该主题已有文档，该更新而不是新建）",
		"weights": map[string]float64{
			"refusal_unsatisfied": docgen.WeightRefused,
			"answer_incomplete":   docgen.WeightIncomplete,
		},
	})
}
