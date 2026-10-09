// Package corpus 是语料的装卸面：从 store 读、往 store 写、从目录导入。
//
// 检索侧的语料一直是内联或从 benchmark 文件来的——HTTP 面要求语料活在
// store 里（端口先行的另一面：collection 是语料的家，索引只是它的投影）。
// 索引可弃、语料不可弃：进程重启后从 store 重建索引，不需要重新导入。
package corpus

import (
	"time"

	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// Collection 是**默认 realm** 的语料集合名。
//
// 为什么要按 realm 分集合（而不是给文档打个 realm 标签）：标签能过滤，
// **忘记过滤的那条路径就漏**。集合是**物理分开**的——查错集合得到的是"没有"，
// 不是"别人的数据"。多个项目/租户共用一个实例时，这是隔离的底线。
const Collection = "documents"

// CollectionFor 返回某 realm 的语料集合名。realm 为空 → 默认集合
// （单机/本地开发的老路径一字不变）。
func CollectionFor(realm string) string {
	r := strings.TrimSpace(realm)
	if r == "" {
		return Collection
	}
	return Collection + "/" + sanitizeRealm(r)
}

// sanitizeRealm 把 realm 洗成安全片段：只留字母数字与 - _ .，其余换 _，
// 长度封顶（集合名会进存储键，不能无限长也不能带路径分隔符）。
func sanitizeRealm(r string) string {
	var b strings.Builder
	for _, r := range r {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	out := b.String()
	if out == "" {
		return "default"
	}
	return out
}

// Doc 是一篇语料文档（store 里的结构体形状）。
type Doc struct {
	ID        string
	Body      string
	Encoding  string `json:"encoding,omitempty"`   // utf-8 / gb18030（摄入时的解码分层）
	SrcDigest string `json:"src_digest,omitempty"` // 原始字节的 sha256（血缘：产物→原始字节）
	SrcBytes  int    `json:"src_bytes,omitempty"`  // 原始字节数

	// 以下是**生成产物**的元数据（docgen 写；手抄文档为空）。
	//
	// 为什么放这里而不是正文里：正文要原样可读（贴进任何地方都成立），
	// 而版本/主题键/来源清单是**机器账**——塞进正文会被索引进倒排
	//（"cumulus version 2" 这类垃圾 token 参与打分）。
	Kind        string    `json:"kind,omitempty"`      // "generated"（手抄为空）
	TopicKey    string    `json:"topic_key,omitempty"` // 主题键（同主题 → 同键 → 同一篇）
	Version     int       `json:"version,omitempty"`   // 第几版（新建=1）
	Sources     []string  `json:"sources,omitempty"`   // 本版依据的源文档 id
	GeneratedAt time.Time `json:"generated_at,omitempty"`
}

// Port 是语料面依赖的 store 能力（窄接口，测试可替）。它就是
// store.Port 的子集——不重新声明，直接用 store.Port 的类型别名语义，
// 测试给个假件即可。
type Port = store.Port

// Load 从 store 读全量语料。空集合返回空切片（不报错——空库是合法状态）。
func Load(ctx gocontext.Context, p Port) ([]retrieval.Document, error) {
	return LoadRealm(ctx, p, "")
}

// LoadRealm 只读**某 realm** 的语料。realm 为空 = 默认集合（单机老路径）。
func LoadRealm(ctx gocontext.Context, p Port, realm string) ([]retrieval.Document, error) {
	coll := CollectionFor(realm)
	ids, err := p.ListIDs(ctx, coll, 0)
	if err != nil {
		return nil, fmt.Errorf("corpus: list: %w", err)
	}
	docs := make([]retrieval.Document, 0, len(ids))
	for _, id := range ids {
		var d Doc
		if err := p.GetStruct(ctx, coll, id, &d); err != nil {
			return nil, fmt.Errorf("corpus: get %s: %w", id, err)
		}
		if d.ID == "" {
			d.ID = id
		}
		docs = append(docs, retrieval.Document{ID: d.ID, Body: d.Body})
	}
	return docs, nil
}

// Save 把语料写进 store（upsert）。
func Save(ctx gocontext.Context, p Port, docs []retrieval.Document) error {
	if err := p.EnsureCollection(ctx, Collection); err != nil {
		return fmt.Errorf("corpus: ensure: %w", err)
	}
	for _, d := range docs {
		if err := p.PutStruct(ctx, Collection, d.ID, Doc{ID: d.ID, Body: d.Body}); err != nil {
			return fmt.Errorf("corpus: put %s: %w", d.ID, err)
		}
	}
	return nil
}

// ImportFile 从 JSONL 导入（每行 {"id": "...", "body": "..."}；也认
// cn-law-rag 的 anchor/positive 形状——positive 当 body，标题进 id 前缀）。
// 返回导入的文档数。已存在的 id 覆盖（upsert）。
func ImportFile(ctx gocontext.Context, p Port, path string) (int, error) {
	return ImportFileRealm(ctx, p, "", path)
}

// ImportFileRealm 是**按 realm** 的 jsonl 导入。
//
// 为什么必须有：原实现硬编码写 `documents`（无 realm 后缀），而 watch 目录在多租户
// 下写的是 `documents/<realm>` —— 于是 "-watch 吃 jsonl 语料 + 配了 CUMULUS_KEYS"
// 的组合下，**导入了 9 篇、查询一篇都读不到**（真跑：长文档探针 0/10 命中，
// 而短条文语料用 .md 所以一直没暴露）。realm 空 = 单租户老路径。
func ImportFileRealm(ctx gocontext.Context, p Port, realm, path string) (int, error) {
	coll := CollectionFor(realm)
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("corpus: open %s: %w", path, err)
	}
	defer f.Close()
	if err := p.EnsureCollection(ctx, coll); err != nil {
		return 0, fmt.Errorf("corpus: ensure: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec struct {
			ID       string `json:"id"`
			Body     string `json:"body"`
			Positive string `json:"positive"` // cn-law-rag 形状
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			return n, fmt.Errorf("corpus: %s: %w", path, err)
		}
		body := rec.Body
		if body == "" {
			body = rec.Positive
		}
		if body == "" {
			continue
		}
		// jsonl 路径的正文也过规范化（文本自 JSON 已解码，只规范空白）；
		// 内容寻址在规范化之后——同一份内容不同空白形态是同一篇
		body = Normalize(body)
		id := rec.ID
		if id == "" {
			id = hashID(body)
		}
		if err := p.PutStruct(ctx, coll, id, Doc{ID: id, Body: body, Encoding: "utf-8", SrcDigest: DigestFull([]byte(body))}); err != nil {
			return n, fmt.Errorf("corpus: put %s: %w", id, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("corpus: scan %s: %w", path, err)
	}
	return n, nil
}

// ImportDir 递归导入目录下全部 .jsonl/.txt/.md（一文一文件的形态）。
func ImportDir(ctx gocontext.Context, p Port, dir string) (int, error) {
	total := 0
	// 递归（laws-full 的形态：法律/司法解释/宪法各占子目录）
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.HasPrefix(info.Name(), ".") {
			return nil
		}
		switch {
		case strings.HasSuffix(info.Name(), ".jsonl"):
			n, ierr := ImportFile(ctx, p, path)
			if ierr != nil {
				return ierr
			}
			total += n
		case strings.HasSuffix(info.Name(), ".txt"), strings.HasSuffix(info.Name(), ".md"):
			// 一文一文件（laws-full 的形态：一法一个 .txt）：整篇为一文档，
			// 内容寻址 id，窗口 machinery 负责篇内 span
			n, ierr := importTextFile(ctx, p, path)
			if ierr != nil {
				return ierr
			}
			total += n
		}
		return nil
	})
	if err != nil {
		return total, fmt.Errorf("corpus: walk %s: %w", dir, err)
	}
	return total, nil
}

// importTextFile 把一个 .txt/.md 文件整篇入库（过摄入管：GBK 转码 +
// 规范化 + 血缘）。一文一文件的形态（laws-full）走这里。
func importTextFile(ctx gocontext.Context, p Port, path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("corpus: read %s: %w", path, err)
	}
	pre, perr := Prepare(raw)
	if perr != nil {
		return 0, fmt.Errorf("corpus: prepare %s: %w", path, perr)
	}
	if err := p.EnsureCollection(ctx, Collection); err != nil {
		return 0, err
	}
	doc := Doc{ID: pre.ID, Body: pre.Body, Encoding: pre.Encoding, SrcDigest: pre.SrcDigest, SrcBytes: pre.SrcBytes}
	if err := p.PutStruct(ctx, Collection, pre.ID, doc); err != nil {
		return 0, fmt.Errorf("corpus: put %s: %w", pre.ID, err)
	}
	return 1, nil
}

func hashID(s string) string {
	const hex = "0123456789abcdef"
	var b [8]byte
	for i := 0; i < len(s); i++ {
		b[i%8] ^= s[i]
	}
	out := make([]byte, 16)
	for i, x := range b {
		out[i*2] = hex[x>>4]
		out[i*2+1] = hex[x&0xf]
	}
	return string(out)
}
