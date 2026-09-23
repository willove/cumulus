#!/usr/bin/env python3
"""Print seed queries (head) to stderr for the probe log."""
import json
import sys

for line in sys.stdin:
    if line.strip():
        print("  ", json.loads(line)["query"][:70], file=sys.stderr)
