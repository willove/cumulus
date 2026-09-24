package adapt

import (
	"context"
	"strings"
	"testing"
)

func collect(t *testing.T, name, in string, f Fields) []Doc {
	t.Helper()
	var out []Doc
	err := Stream(context.Background(), name, strings.NewReader(in), f, func(d Doc) error {
		out = append(out, d)
		return nil
	})
	if err != nil {
		t.Fatalf("stream %s: %v", name, err)
	}
	return out
}

// The baidu_baike / people's-daily shape: one object per line inside a .json file.
func TestJSONLinesInAJSONFile(t *testing.T) {
	in := `{"title":"灯草塘村","content":"隶属于云南省曲靖市。"}
{"subTitle":"李强总理监誓","dataTime":"2024-12-24","contentText":"新华社北京12月23日电。"}
`
	got := collect(t, "test.json", in, Fields{})
	if len(got) != 2 {
		t.Fatalf("want 2 docs, got %d: %+v", len(got), got)
	}
	if got[0].Title != "灯草塘村" || got[0].Body != "隶属于云南省曲靖市。" {
		t.Fatalf("doc0: %+v", got[0])
	}
	if got[1].Title != "李强总理监誓" || !strings.Contains(got[1].Body, "新华社") {
		t.Fatalf("doc1 (contentText autodetect): %+v", got[1])
	}
	// The raw JSON braces must never leak into the body.
	for _, d := range got {
		if strings.HasPrefix(d.Body, "{") || strings.Contains(d.Body, `":"`) {
			t.Fatalf("body is raw JSON: %q", d.Body)
		}
	}
}

// The poetry shape: a top-level array whose records carry a paragraphs array.
func TestJSONArrayParagraphs(t *testing.T) {
	in := `[
 {"chapter":"梁惠王上","paragraphs":["孟子見梁惠王。","王曰：叟不遠千里而來。"]},
 {"chapter":"公孫丑下","paragraphs":["孟子自齊葬於魯。"]}
]`
	got := collect(t, "mengzi.json", in, Fields{})
	if len(got) != 2 {
		t.Fatalf("want 2 docs, got %d", len(got))
	}
	if !strings.Contains(got[0].Body, "孟子見梁惠王。") || !strings.Contains(got[0].Body, "王曰") {
		t.Fatalf("paragraphs must join with newlines: %q", got[0].Body)
	}
	if strings.Count(got[0].Body, "\n") != 1 {
		t.Fatalf("two paragraphs = one newline: %q", got[0].Body)
	}
	if got[0].Title != "梁惠王上" {
		t.Fatalf("chapter should be the title: %+v", got[0])
	}
}

// An explicit spec must win over autodetection.
func TestExplicitFieldsWin(t *testing.T) {
	in := `{"a":"问句","b":"法条正文内容在这里","c":"x"}`
	got := collect(t, "x.jsonl", in, Fields{ID: "a", Title: "a", Body: "b", Extra: []string{"c"}})
	if len(got) != 1 {
		t.Fatalf("want 1 doc")
	}
	if got[0].Key != "问句" || got[0].Body != "法条正文内容在这里" {
		t.Fatalf("spec ignored: %+v", got[0])
	}
	if got[0].Meta["c"] != "x" {
		t.Fatalf("extra passthrough: %+v", got[0].Meta)
	}
}

// A pretty-printed single object is one document, not zero and not an error.
func TestSinglePrettyObject(t *testing.T) {
	in := "{\n  \"title\": \"T\",\n  \"content\": \"正文\"\n}\n"
	got := collect(t, "one.json", in, Fields{})
	if len(got) != 1 || got[0].Title != "T" {
		t.Fatalf("single object: %+v", got)
	}
}

// Records with no readable text are skipped, not turned into empty documents.
func TestBodylessRecordsSkipped(t *testing.T) {
	in := `{"title":"无正文"}
{"title":"有","content":"正文"}`
	got := collect(t, "x.jsonl", in, Fields{})
	if len(got) != 1 || got[0].Body != "正文" {
		t.Fatalf("bodyless record must be skipped: %+v", got)
	}
}

// The million-product-title shape: text_id,text with a header.
func TestCSVProductTitles(t *testing.T) {
	in := "text_id,text\n1,红棉优级小粒老黄冰糖1.2kg大罐\n2,异形魔方顺滑风火轮移棱\n"
	got := collect(t, "corpus.csv", in, Fields{})
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d: %+v", len(got), got)
	}
	if got[0].Key != "1" || got[0].Body != "红棉优级小粒老黄冰糖1.2kg大罐" {
		t.Fatalf("row0: %+v", got[0])
	}
	if got[0].Meta["text_id"] != "1" {
		t.Fatalf("meta passthrough: %+v", got[0].Meta)
	}
}

// A topic-style CSV with a query column must not use that column as the body.
func TestCSVPicksWidestColumn(t *testing.T) {
	in := "query_id,query,document\n1,连接池,连接池最大 128，超时 30 秒，属于关键配置\n"
	got := collect(t, "q.csv", in, Fields{})
	if len(got) != 1 {
		t.Fatalf("want 1 row")
	}
	if got[0].Body != "连接池最大 128，超时 30 秒，属于关键配置" {
		t.Fatalf("body must be the widest column: %+v", got[0])
	}
}

// An unknown container is an error that names the file, never a silent empty.
func TestUnknownShapeIsAnError(t *testing.T) {
	err := Stream(context.Background(), "weird.bin", strings.NewReader("\x00\x01\x02binary"), Fields{},
		func(Doc) error { return nil })
	if err == nil {
		t.Fatal("an unrecognized container must fail loudly")
	}
	if !strings.Contains(err.Error(), "weird.bin") {
		t.Fatalf("the error must name the file: %v", err)
	}
}

// A malformed JSON line must surface, not be skipped as an empty record.
func TestMalformedJSONIsAnError(t *testing.T) {
	err := Stream(context.Background(), "bad.jsonl", strings.NewReader(`{"a":1}`+"\n"+`{"a":`), Fields{},
		func(Doc) error { return nil })
	if err == nil {
		t.Fatal("a truncated record must fail")
	}
}

// Cancellation is honoured between records, not after the whole file.
func TestStreamHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	err := Stream(ctx, "x.jsonl", strings.NewReader(`{"content":"a"}`+"\n"+`{"content":"b"}`), Fields{},
		func(Doc) error {
			n++
			cancel()
			return nil
		})
	if err == nil || err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if n != 1 {
		t.Fatalf("must stop at the cancelled record, emitted %d", n)
	}
}

// Identity is stable across runs (idempotent re-ingest) and independent of map
// iteration order.
func TestKeyIsDeterministic(t *testing.T) {
	in := `{"zz":"1","aa":"2","content":"正文"}`
	first := collect(t, "x.jsonl", in, Fields{})
	for i := 0; i < 20; i++ {
		again := collect(t, "x.jsonl", in, Fields{})
		if again[0].Key != first[0].Key {
			t.Fatalf("key unstable: %q vs %q", again[0].Key, first[0].Key)
		}
	}
}

// A chat corpus ({"messages":[{role,content}]}) must render as readable turns.
// It previously produced ZERO documents because flatten returned "" for objects.
func TestChatMessagesRenderAsTurns(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"你是客服"},{"role":"user","content":"要买一把茶刀"}]}
{"messages":[{"role":"user","content":"退货政策是什么"},{"role":"assistant","content":"七天无理由。"}]}`
	got := collect(t, "dev.jsonl", in, Fields{})
	if len(got) != 2 {
		t.Fatalf("chat records must produce documents, got %d: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Body, "user: 要买一把茶刀") {
		t.Fatalf("turns must be labelled: %q", got[0].Body)
	}
	if !strings.Contains(got[0].Body, "system: 你是客服") {
		t.Fatalf("system turn must survive: %q", got[0].Body)
	}
}

// Identity must be UNIQUE per record. Deriving it from "the alphabetically first
// short field" collapsed every People's-Daily article onto dataTime and every
// poem onto its author — thousands of records evicting each other as revisions.
func TestKeyIsUniquePerRecord(t *testing.T) {
	// Same dataTime on every record (the real People's-Daily shape).
	in := `{"dataTime":"2024-12-24","subTitle":"甲","contentText":"正文一"}
{"dataTime":"2024-12-24","subTitle":"乙","contentText":"正文二"}
{"dataTime":"2024-12-24","subTitle":"丙","contentText":"正文三"}`
	got := collect(t, "news.json", in, Fields{})
	if len(got) != 3 {
		t.Fatalf("want 3 docs, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d.Key] {
			t.Fatalf("duplicate key %q — records would evict each other", d.Key)
		}
		seen[d.Key] = true
	}
	// An explicit id field still wins and stays stable.
	got2 := collect(t, "x.jsonl", `{"id":"b1","title":"t","content":"c"}`, Fields{})
	if got2[0].Key != "b1" {
		t.Fatalf("explicit id must win: %q", got2[0].Key)
	}
}
