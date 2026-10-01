#!/usr/bin/env bash
# Offline-gate isolation. SOURCE THIS from any harness that must run against the
# deterministic offline stubs (scripts/e2e.sh, scenarios/run.sh, future gates).
#
# Why two layers, and why a file at all:
#
#   CLUS_ENV=/dev/null alone is NOT enough. The operator convention (see
#   .env.example) ALSO exports LLM_BASE_URL / LLM_API_KEY / LLM_MODEL_NAME
#   straight into the shell, and applyLLMAliases (cmd/cumulus-cluster/env.go)
#   promotes those into AIGATE_*. Every search then reaches the live endpoint:
#   with no key the assertions collapse on `llm: status 401` (e2e went 153 → 72
#   ok / 81 fail; scenarios went 13 → 4 ok), and WITH a key they silently burn
#   real tokens and stop being reproducible — exactly what D6 forbids
#   ("机制正确性与模型质量必须分门").
#
# So: unset the ambient variables AND pin CLUS_OFFLINE, which the binary honors
# regardless of configuration. Belt and braces, because either alone leaks.
#
# Measurement harnesses (scripts/endpoint-probe.sh, realeval.sh, goldeval.sh,
# theta-probe.sh, r2-cluster-probe.sh, realdata-probe.sh) deliberately talk to a
# live endpoint and must NOT source this file.
#
# CLUS_MINILM_REQUIRE is unset as well: a strict-gate harness sets it per
# command, and an inherited value would hard-fail unrelated offline runs.
#
# The MiniLM weights get the same treatment, for the same reason. They live
# at $HOME/.cumulus/models (or wherever the developer points CLUS_MODEL_DIR),
# and the embedder picks them up silently — `ensure -embed` then reports
# minilm-l12-384 instead of local-hash-64 and, worse, builds a 384-dim
# body_embed index on the gate's own store. Any later gate that asserts the
# hash-64 degradation path then dies on cumulite's VECTOR_DIM_MISMATCH, so
# one leaked weight turns a deterministic gate environment into a cascade.
# This is exactly what the e2e assertion means by "不许静默走语义模型导致门
# 结果随权重漂移" — the intent was right, nothing enforced it.
#
# CLUS_MODEL_DIR (canonical) is EXPORTED rather than unset: no offline gate
# wants the developer's real weights, and DefaultDir checks it before the
# legacy CLUS_MINILM_DIR, so this shadows any ambient value. The path is
# namespaced under TMPDIR and is never created — Available() only looks for
# model.safetensors under it.
unset LLM_BASE_URL LLM_API_KEY LLM_MODEL_NAME
unset LLM_BASE_URL LLM_API_KEY LLM_CHAT_MODEL LLM_EMBED_MODEL LLM_REASONING_SPLIT
unset CLUS_EMBED CLUS_MINILM_REQUIRE
unset CLUS_MINILM_DIR
export CLUS_MODEL_DIR="${TMPDIR:-/tmp}/cumulus-offline-no-weights"
export CLUS_ENV=/dev/null
export CLUS_OFFLINE=1
