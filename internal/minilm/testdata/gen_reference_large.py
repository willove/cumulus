# Print the expanded reference set for the pure-Go MiniLM validation (JSON on
# stdout in two marked sections). Usage:
#   <venv-python> gen_reference_large.py <model_dir> [corpus_dir] > refs.jsonl
# Shell side splits the sections into testdata/large_reference.json and
# testdata/fuzz_ids.json. Deterministic: fixture selection uses an explicit
# LCG below (reproducibility requirement, NOT a security mechanism).
import json
import os
import sys

from sentence_transformers import SentenceTransformer


def safe_model_dir(p):
    rp = os.path.normpath(os.path.abspath(os.path.expanduser(p)))
    if ".." in rp.split(os.sep):
        raise SystemExit("path must not traverse: " + p)
    if not os.path.isfile(os.path.join(rp, "model.safetensors")):
        raise SystemExit("not a MiniLM model dir: " + rp)
    return rp


def here():
    return os.path.dirname(os.path.abspath(__file__))


model_dir = safe_model_dir(sys.argv[1])
if len(sys.argv) > 2:
    corpus_dir = safe_model_dir(sys.argv[2])
else:
    corpus_dir = os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(here()))),
                              "var", "realeval")

seed_texts = [
    "连接池最大连接数是 128",
    "部署机房在广州，灾备在上海",
    "The buffer pool is limited to 128 connections.",
    "照明系统主灯功率 200 瓦，色温 4000K",
    "专利法主要是为了什么目的制定的？",
    "hello world",
    "2026-09-23 10:00:00",
    "a  b",
]


def corpus_texts(want):
    out = []
    for name in ("items.jsonl", "corpus.jsonl"):
        p = os.path.join(corpus_dir, name)
        if not os.path.isfile(p):
            continue
        with open(p, encoding="utf-8") as fh:
            for line in fh:
                try:
                    r = json.loads(line)
                except Exception:
                    continue
                for key in ("query", "text"):
                    v = r.get(key)
                    if v:
                        out.append(v[:600])
    return out[:want]


texts = list(seed_texts) + corpus_texts(92)

# Deterministic LCG (fixture selection only — reproducibility, not crypto).
_state = 42


def lcg_next(mod):
    global _state
    _state = (_state * 6364136223846793005 + 1442695040888963407) % (1 << 64)
    return _state % mod


fuzz_pool = [t for t in texts if len(t) >= 8]
fuzz = []
for _ in range(300):
    base = fuzz_pool[lcg_next(len(fuzz_pool))]
    i = lcg_next(max(1, len(base) - 4))
    j = min(len(base), i + 3 + lcg_next(77))
    fuzz.append(base[i:j])
alphabet = "abcXYZ019 ,.;:!?（）「」、。的了吗吧 le département über 灯池表 🔧 café"
for _ in range(200):
    n = 2 + lcg_next(38)
    fuzz.append("".join(alphabet[lcg_next(len(alphabet))] for _ in range(n)))

model = SentenceTransformer(model_dir, device="cpu")
tok = model.tokenizer

large = []
for t in texts:
    enc = tok(t, truncation=True, max_length=128, return_attention_mask=True)
    emb = model.encode([t], normalize_embeddings=True, convert_to_numpy=True)[0]
    large.append({
        "text": t,
        "input_ids": [int(x) for x in enc["input_ids"]],
        "embedding": [round(float(x), 6) for x in emb],
    })
fuzz_out = []
for t in fuzz:
    enc = tok(t, truncation=True, max_length=128, return_attention_mask=True)
    fuzz_out.append({"text": t, "input_ids": [int(x) for x in enc["input_ids"]]})

print("=== LARGE ===")
print(json.dumps(large, ensure_ascii=False))
print("=== FUZZ ===")
print(json.dumps(fuzz_out, ensure_ascii=False))
print("counts:", len(large), len(fuzz_out), "| id lens:", [len(o["input_ids"]) for o in large[:6]],
      file=sys.stderr)
