package corpus

// Prepared 是摄入管的产物：规范化正文 + 血缘。顺序：**解码 → 规范化 →
// 内容寻址**——先规范化后哈希，同一内容换了编码也是同一篇；解码在规
// 范化之前，否则空白折叠作用在乱码上。
type Prepared struct {
	ID        string // 规范化后的内容 id
	Body      string // 规范化后的 UTF-8 正文
	Encoding  string // 解码分层：utf-8 / gb18030
	SrcDigest string // 原始字节的 sha256（血缘：产物 → 原始字节）
	SrcBytes  int    // 原始字节数
}

// Prepare 是摄入管的统一定点：raw bytes → 可入库的文档字段。不可解码
// 返回 ErrUndecodable——**拒收，不静默替换**（把 GBK 当 UTF-8 存进去，
// 每字变一个 U+FFFD，文档能答查询但不可读，且没有任何一环会报告）。
func Prepare(raw []byte) (Prepared, error) {
	dec, err := Decode(raw)
	if err != nil {
		return Prepared{}, err
	}
	body := Normalize(dec.Text)
	if body == "" {
		return Prepared{}, ErrEmptyBody
	}
	return Prepared{
		ID:        hashID(body), // 规范化后哈希
		Body:      body,
		Encoding:  string(dec.Tier),
		SrcDigest: DigestFull(raw),
		SrcBytes:  dec.SrcBytes,
	}, nil
}

// ErrEmptyBody 解码正常但规范化后为空（纯空白文档）。
var ErrEmptyBody = errString("corpus: empty body after normalize")

type errString string

func (e errString) Error() string { return string(e) }
