#!/usr/bin/env python3
"""Build an ask ingest corpus from a directory of plain-text statute files.

Chinese_Law layout (KuugoRen/Chinese_Law): one file per statute, one article
per non-empty line. Keys are deterministic so gold_sources stay stable across
rebuilds (must match testdata/eval/chinalaw39.jsonl):

  law key   = L{6 + rank of filename in sorted(*.txt):03d}
  article   = A{global sequential index over all non-empty lines:05d}
              (A is corpus-wide, not per-statute — L007's first line is A00038)
  title     = <statute short name>·<第X条 if present>

Law offset 6 is the golden-set convention (first statute file sorts to L006).
This is a keying convention, not corpus-specific scoring.

Usage:
  python3 scripts/chinalaw_corpus.py --dir DATADIR --out corpus.jsonl
"""
from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path

ARTICLE_RE = re.compile(r"第[一二三四五六七八九十百千万零〇两]+条")
# Historic offset: first sorted statute is L006 in the golden set.
LAW_OFFSET = 6


def short_name(stem: str) -> str:
    for prefix in ("中华人民共和国", "中华人民共和"):
        if stem.startswith(prefix):
            return stem[len(prefix) :]
    return stem


def build(dir_path: Path) -> list[dict]:
    files = sorted(p for p in dir_path.glob("*.txt") if p.is_file())
    if not files:
        raise SystemExit(f"no .txt statutes under {dir_path}")
    out: list[dict] = []
    global_ai = 0
    for fi, path in enumerate(files):
        law_key = f"L{LAW_OFFSET + fi:03d}"
        short = short_name(path.stem)
        articles = [
            ln.strip()
            for ln in path.read_text(encoding="utf-8").splitlines()
            if ln.strip()
        ]
        for text in articles:
            m = ARTICLE_RE.search(text)
            art = m.group(0) if m else f"第{global_ai + 1}条"
            out.append(
                {
                    "key": f"{law_key}-A{global_ai:05d}",
                    "title": f"{short}·{art}",
                    "text": text,
                }
            )
            global_ai += 1
    return out


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--dir", required=True, help="directory of statute .txt files")
    ap.add_argument("--out", required=True, help="output corpus.jsonl path")
    args = ap.parse_args()
    rows = build(Path(args.dir))
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w", encoding="utf-8") as f:
        for r in rows:
            f.write(json.dumps(r, ensure_ascii=False) + "\n")
    laws = len({r["key"].split("-")[0] for r in rows})
    print(f"articles={len(rows)} laws={laws} → {out}", file=sys.stderr)


if __name__ == "__main__":
    main()
