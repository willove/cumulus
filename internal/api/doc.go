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
	"strings"

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
		// 落库走摄入面（内容哈希 id + 语料规范化），这样生成文档与手抄进来的
		// 文档在**同一个索引口径**里，不会两套身份。
		//
		// **索引优先级**：生成文档的检索权重压到源文档之下（Generated 标记 +
		// retrieval 的 Prior 升权规则）。理由是实测出来的：生成文档是**源的转述**，
		// 词面重叠、篇幅更长，BM25 打分天然比不过源——不压优先级的话，问答永远
		// 引用源文档，"整理一次、问答受益"就只停在"库里多了一篇"。
		//
		// 压优先级不等于压掉：命中生成文档时它仍然可被引用（**先答不上来时，
		// 转述文档是更好的答案**——它已经把多篇源拼在一起了）。
		doc.Body = docgen.ApplyMarker(doc.Body)
		// id 用 gen- 前缀（内容哈希 + 前缀）：检索期能 O(1) 认出"这是转述文档"并
		// 降权一档。**一次写入**（不"先按哈希写再改名"——那是两写一删，中途
		// 失败会留下两篇）。
		id := genPrefix + corpus.DigestHex([]byte(doc.Body))[:12]
		if perr := s.putDoc(ctx, realm, id, doc.Body); perr != nil {
			writeErr(w, http.StatusInternalServerError, "store: "+perr.Error())
			return
		}
		out.ID = id
		out.Stored = true
		// 写完要让本 realm 的索引失效：否则"生成成功了、下一轮却查不到"。
		s.InvalidateRealm(realm)
		if _, ierr := s.IndexFor(r.Context(), realm); ierr == nil {
			out.IndexRebuilt = true
		}
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
func (s *Server) putDoc(ctx gocontext.Context, realm, id, body string) error {
	coll := corpus.CollectionFor(realm)
	if err := s.Store.EnsureCollection(ctx, coll); err != nil {
		return err
	}
	return s.Store.PutStruct(ctx, coll, id, corpus.Doc{
		ID: id, Body: body, Encoding: "text/plain; charset=utf-8",
	})
}
