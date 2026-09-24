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

unset LLM_BASE_URL LLM_API_KEY LLM_MODEL_NAME
unset AIGATE_BASE_URL AIGATE_API_KEY AIGATE_CHAT_MODEL AIGATE_EMBED_MODEL AIGATE_REASONING_SPLIT
unset CLUS_EMBED CLUS_MINILM_REQUIRE
export CLUS_ENV=/dev/null
export CLUS_OFFLINE=1
