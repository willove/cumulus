#!/usr/bin/env python3
"""bench run: 在数据快照的私有副本上跑场景集，记录每问指标。

用法:
  python3 scripts/bench/run.py --tag on                      # 默认环境（账本开）
  python3 scripts/bench/run.py --tag off --env CLUS_AFFINITY=0   # 对照组
  python3 scripts/bench/run.py --tag tau7 --env CLUS_AFFINITY_TAU=7

每个 run 从 scripts/bench/base 快照拷贝一份数据（没有就现做），所以任何两次
run 的起始状态完全一致——账本/会话栈都不会把上一次 run 的学习带进来。私有端口
8610，不碰使用者的 8484。

输出: scripts/bench/results-<tag>.json
"""
import argparse
import json
import os
import shutil
import signal
import subprocess
import sys
import time
import urllib.request
import uuid

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
BENCH = os.path.join(ROOT, "scripts", "bench")
BASE = os.path.join(BENCH, "base")
PORT = 8610
URL = f"http://127.0.0.1:{PORT}"


def post(url, payload, timeout=300):
    req = urllib.request.Request(url, data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def wait_health(proc, seconds=60):
    deadline = time.time() + seconds
    while time.time() < deadline:
        if proc.poll() is not None:
            raise SystemExit(f"serve exited early: {proc.returncode}")
        try:
            with urllib.request.urlopen(URL + "/health", timeout=2) as r:
                if r.status == 200:
                    return
        except Exception:
            time.sleep(0.3)
    raise SystemExit("serve did not become healthy")


def snapshot():
    """把当前 var/cumulus-cluster 冻结成实验基线（首次或 --refresh）。"""
    src = os.path.join(ROOT, "var", "cumulus-cluster")
    if not os.path.isdir(src):
        raise SystemExit(f"data dir not found: {src}")
    if os.path.exists(BASE):
        shutil.rmtree(BASE)
    ignore = shutil.ignore_patterns("LOCK", "*.lock")
    shutil.copytree(src, BASE, ignore=ignore)
    print(f"[bench] snapshot → scripts/bench/base ({os.path.getsize(BASE) and 'ok'})")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", required=True)
    ap.add_argument("--env", action="append", default=[])
    ap.add_argument("--scenarios", default=os.path.join(BENCH, "scenarios.json"))
    ap.add_argument("--refresh-snapshot", action="store_true")
    ap.add_argument("--reset", action="store_true",
                    help="reset the copy's learning state first (clusters/ledger/evidence/sessions) — the clean-start arm")
    ap.add_argument("--passes", type=int, default=1,
                    help="run the scenario set N times against the SAME data (no reset between passes) — the 越用越快 curve")
    ap.add_argument("--port", type=int, default=PORT)
    args = ap.parse_args()

    if args.refresh_snapshot or not os.path.isdir(BASE):
        snapshot()

    global URL
    port = args.port
    URL = f"http://127.0.0.1:{port}"

    data = os.path.join(BENCH, f"run-{args.tag}")
    if os.path.exists(data):
        shutil.rmtree(data)
    shutil.copytree(BASE, data, ignore=shutil.ignore_patterns("LOCK", "*.lock"))

    # 干净起点：reset 掉这份拷贝学过的所有东西（语料不动）。记录 reset 前后
    # 的学习状态，让“这轮从多干净开始”成为结果的一部分而不是口头承诺。
    start_state = None
    if args.reset:
        rc = subprocess.run([os.path.join(ROOT, "bin", "cumulus-cluster"), "-data", data,
                             "reset", "learned", "-yes", "-ns", "chinalaw",
                             "-evidence", "clus_evidence"],
                            cwd=ROOT, capture_output=True, text=True)
        if rc.returncode != 0:
            raise SystemExit(f"reset failed: {rc.stderr[:300]}")
    rc = subprocess.run([os.path.join(ROOT, "bin", "cumulus-cluster"), "-data", data,
                         "learning", "-ns", "chinalaw", "-evidence", "clus_evidence"],
                        cwd=ROOT, capture_output=True, text=True)
    if rc.returncode == 0:
        try:
            start_state = json.loads(rc.stdout)
        except Exception:
            pass
    print(f"[bench] start learning state: clean={start_state.get('clean') if start_state else '?'} "
          f"total={start_state.get('total') if start_state else '?'}")

    env = dict(os.environ)
    env["CLUS_SEARCH_TOKEN_BUDGET"] = "80000"
    for kv in args.env:
        k, _, v = kv.partition("=")
        env[k] = v
    binpath = os.path.join(ROOT, "bin", "cumulus-cluster")
    proc = subprocess.Popen(
        [binpath, "-ns", "chinalaw", "-sources", "clus_sources", "-evidence", "clus_evidence",
         "-data", data, "serve", "-listen", f"127.0.0.1:{PORT}"],
        cwd=ROOT, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        start_new_session=True)
    try:
        wait_health(proc)
        spec = json.load(open(args.scenarios))
        ns = spec.get("ns", "chinalaw")
        runs = []
        for pass_no in range(1, max(1, args.passes) + 1):
            if args.passes > 1:
                st = subprocess.run([os.path.join(ROOT, "bin", "cumulus-cluster"), "-data", data,
                                     "learning", "-ns", "chinalaw", "-evidence", "clus_evidence"],
                                    cwd=ROOT, capture_output=True, text=True)
                learned = "?"
                if st.returncode == 0:
                    try:
                        learned = json.loads(st.stdout).get("total")
                    except Exception:
                        pass
                print(f"[bench] ===== pass {pass_no}/{args.passes} (已学状态 total={learned}) =====")
            for sc in spec["scenarios"]:
                session = uuid.uuid4().hex[:12]
                rows = []
                for q in sc["queries"]:
                    t0 = time.time()
                    d = post(URL + "/v1/search", {"query": q, "session": session, "prior": True, "ns": ns})
                    wall = int((time.time() - t0) * 1000)
                    ans = d.get("answer") or {}
                    refs = (d.get("citations") or {}).get("refs") or []
                    rows.append({
                        "pass": pass_no,
                        "query": q, "wall_ms": wall, "server_ms": d.get("latency_ms", 0),
                        "mode": d.get("mode"), "reused": bool(d.get("reused")),
                        "conf": round(ans.get("confidence", 0), 3),
                        "loops": d.get("loops", 0), "widened": d.get("widened", 0),
                        "stop": d.get("stop_reason", ""),
                        "insufficient": "证据不足" in (ans.get("summary") or ""),
                        "docs": sorted({r.get("source_id", "") for r in refs}),
                        "refs": len(refs),
                        "tokens": d.get("tokens", 0),
                    })
                    print(f"[{sc['name']}#p{pass_no}] {q[:24]:<26} {wall:>7}ms conf={rows[-1]['conf']:.2f} "
                          f"{rows[-1]['mode']}/{rows[-1]['stop'] or '-'} refs={rows[-1]['refs']}"
                          + ("  ⚠ insufficient" if rows[-1]["insufficient"] else ""))
                runs.append({"name": sc["name"], "tag": sc.get("tag", ""), "pass": pass_no, "session": session, "rows": rows})
        rev = subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=ROOT,
                             capture_output=True, text=True).stdout.strip()
        out = {"tag": args.tag, "env": args.env, "ns": ns, "runs": runs,
               "git_rev": rev, "reset": bool(args.reset),
               "start_learning": start_state,
               "at": time.strftime("%Y-%m-%d %H:%M:%S")}
        dest = os.path.join(BENCH, f"results-{args.tag}.json")
        json.dump(out, open(dest, "w"), ensure_ascii=False, indent=1)
        rc2 = subprocess.run([os.path.join(ROOT, "bin", "cumulus-cluster"), "-data", data,
                              "learning", "-ns", "chinalaw", "-evidence", "clus_evidence"],
                             cwd=ROOT, capture_output=True, text=True)
        if rc2.returncode == 0:
            try:
                out["end_learning"] = json.loads(rc2.stdout)
                print(f"[bench] end learning state: total={out['end_learning'].get('total')} (taught this run)")
            except Exception:
                pass
        json.dump(out, open(dest, "w"), ensure_ascii=False, indent=1)
        print(f"[bench] wrote {dest}")
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()


if __name__ == "__main__":
    main()
