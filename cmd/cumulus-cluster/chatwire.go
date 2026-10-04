package main

// chatwire — the OpenAI-compatible chat face's wire layer.
//
// POST /v1/chat/completions streams ONE cumulus search as
// chat.completion.chunk frames so any component speaking the OpenAI chat
// protocol (evoke-chat's openai adapter, generic SDKs) can drive the engine:
//
//   - answer deltas        → choices[0].delta.content
//   - retrieval progress   → cumulus extension frames (kind:"stage"/"file")
//   - citations/stats      → cumulus extension frames (kind:"citations"/"done")
//   - stream-end resynth   → cumulus {kind:"replace"} (chunk protocol has no
//                            unsend; the client folds it as a full-content
//                            assistant/message)
//   - finish               → finish_reason:"stop" + usage{total_tokens} + [DONE]
//
// Extension frames carry the RAW internal event payload under "payload" and
// ride on chunks with NO choices, so stock OpenAI parsers skip them untouched.
// The request side takes {model?, messages[], stream, session?, ns?, user?}:
// query = last user message, history = prior user messages (the same six-turn
// fold the search face applies server-side via the session store).

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// wireFunc translates one internal search event into SSE bytes to write.
// "" writes nothing (the chat face drops heartbeats and file noise that the
// search face surfaces as status events).
type wireFunc func(event string, data any) string

// identityWire is the /v1/search/stream wire: named events, JSON data —
// byte-for-byte what the face has always emitted.
func identityWire(event string, data any) string {
	b, _ := json.Marshal(data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, b)
}

// chatChunk marshals one OpenAI-style data frame.
func chatChunk(v any) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

// chatExt frames an internal event payload as a cumulus extension chunk.
func chatExt(kind string, payload any) string {
	return chatChunk(map[string]any{"cumulus": map[string]any{"kind": kind, "payload": payload}})
}

// newChatWire builds the stateful translator: the assistant role frame goes
// out once before the first visible frame; "done" closes the stream with the
// finish chunk, the usage echo and the sentinel.
func newChatWire() wireFunc {
	roleSent := false
	withRole := func(frames string) string {
		if frames == "" || roleSent {
			return frames
		}
		roleSent = true
		return chatChunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}}}}) + frames
	}
	return func(event string, data any) string {
		m, _ := data.(map[string]any)
		switch event {
		case "status":
			if m == nil {
				return ""
			}
			// Stage completions carry the timeline; the synthesizer's
			// chain-of-thought streams as its own kind; started/file/working
			// stay off the chat wire (the UI's live bar reads them from
			// extensions or the heartbeat comments).
			if m["stage"] == "stage" {
				return withRole(chatExt("stage", m))
			}
			if m["stage"] == "reasoning" {
				return withRole(chatExt("reasoning", m))
			}
			return ""
		case "content":
			if m == nil {
				return ""
			}
			if m["replace"] == true {
				return withRole(chatExt("replace", m))
			}
			return withRole(chatChunk(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": m["text"]}}},
			}))
		case "citations":
			return withRole(chatExt("citations", data))
		case "error":
			// The chunk protocol has no error frame; surface it as an extension
			// and still close politely so generic clients terminate.
			return chatExt("error", data) +
				chatChunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}) +
				"data: [DONE]\n\n"
		case "done":
			if m == nil {
				m = map[string]any{}
			}
			payload := map[string]any{}
			for k, v := range map[string]any{
				"mode": m["mode"], "loops": m["loops"], "conf": m["conf"],
				"coverage": m["coverage"], "reused": m["reused"],
				"cluster_id": m["cluster_id"], "tokens": m["tokens"],
				"latency_ms": m["latency_ms"], "widened": m["widened"],
				"stop_reason": m["stop_reason"], "refused": m["refused"],
				"skipped": m["skipped"], "stages": m["stages"],
				"session": m["session"], "session_error": m["session_error"],
			} {
				if v != nil {
					payload[k] = v
				}
			}
			if st, ok := m["stages_tokens"]; ok && st != nil {
				payload["stages_tokens"] = st
			}
			return withRole(chatChunk(map[string]any{
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]any{"total_tokens": m["tokens"]},
			})) + chatChunk(map[string]any{"cumulus": map[string]any{"kind": "done", "payload": payload}}) + "data: [DONE]\n\n"
		}
		return ""
	}
}

// chatCompletionsIn is the request side of the OpenAI chat protocol plus the
// cumulus extensions (session/ns ride as extra body fields; `user` doubles as
// the session id when session is absent, matching the field's intent).
type chatCompletionsIn struct {
	Model    string            `json:"model"`
	Messages []chatWireMessage `json:"messages"`
	Stream   *bool             `json:"stream"`
	Session  string            `json:"session"`
	NS       string            `json:"ns"`
	User     string            `json:"user"`
}

type chatWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// toSearchIn folds the chat body into the search face's input: query is the
// LAST user message, history the prior user turns (oldest → newest, capped at
// the same six turns the session store folds server-side).
func (in chatCompletionsIn) toSearchIn() (searchIn, bool) {
	var users []string
	for _, m := range in.Messages {
		if m.Role == "user" && m.Content != "" {
			users = append(users, m.Content)
		}
	}
	if len(users) == 0 {
		return searchIn{}, false
	}
	query := users[len(users)-1]
	history := users[:len(users)-1]
	if len(history) > 6 {
		history = history[len(history)-6:]
	}
	session := in.Session
	if session == "" {
		session = in.User
	}
	return searchIn{Query: query, History: history, Session: session, Prior: true, NS: in.NS}, true
}

// parseChatCompletions decodes and validates: POST only, stream required (the
// non-stream JSON face is /v1/search), at least one user message.
func parseChatCompletions(w http.ResponseWriter, r *http.Request) (chatCompletionsIn, bool) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST only"})
		return chatCompletionsIn{}, false
	}
	var in chatCompletionsIn
	if err := decode(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return chatCompletionsIn{}, false
	}
	if in.Stream != nil && !*in.Stream {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "stream=true required（非流式请用 /v1/search）"})
		return chatCompletionsIn{}, false
	}
	if _, ok := in.toSearchIn(); !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "messages 需要至少一条 user 消息"})
		return chatCompletionsIn{}, false
	}
	return in, true
}
