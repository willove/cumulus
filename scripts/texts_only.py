#!/usr/bin/env python3
"""Emit the article texts of a corpus.jsonl, one per line (probe build queries)."""
import json
import sys
from pathlib import Path

for line in Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    if line.strip():
        print(json.loads(line)["text"])
