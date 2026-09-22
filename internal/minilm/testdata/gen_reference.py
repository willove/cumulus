# Print reference token IDs + embeddings (JSON) for the pure-Go MiniLM spike.
# Usage: <venv-python> gen_reference.py <model_dir>  >  reference.json
# Requires sentence-transformers (sirchmunk venv has it). Dev-only tool.
import json
import os
import sys

from sentence_transformers import SentenceTransformer


def safe_model_dir(p: str) -> str:
    rp = os.path.normpath(os.path.abspath(os.path.expanduser(p)))
    if ".." in rp.split(os.sep):
        raise SystemExit("path must not traverse: " + p)
    if not os.path.isfile(os.path.join(rp, "model.safetensors")):
        raise SystemExit("not a MiniLM model dir: " + rp)
    return rp


model_dir = safe_model_dir(sys.argv[1])

texts = [
    "连接池最大连接数是 128",
    "专利法主要是为了什么目的制定的？",
    "Hello, World! This is a test.",
    "照明系统主灯功率 200 瓦，色温 4000K",
    "a  b",
    "  leading and trailing  ",
    "URLs like example.com/path are common",
    "超长文本：" + ("数据库索引与查询优化。" * 40),
]

model = SentenceTransformer(model_dir, device="cpu")
tok = model.tokenizer
out = []
for t in texts:
    enc = tok(t, truncation=True, max_length=128, return_attention_mask=True)
    emb = model.encode([t], normalize_embeddings=True, convert_to_numpy=True)[0]
    out.append({
        "text": t,
        "input_ids": [int(x) for x in enc["input_ids"]],
        "attention_mask": [int(x) for x in enc["attention_mask"]],
        "embedding": [round(float(x), 6) for x in emb],
    })

print(json.dumps(out, ensure_ascii=False))
