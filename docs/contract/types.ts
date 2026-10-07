// 从 Go 结构体反射生成（cmd/contract-gen）——改形状先改 Go 代码，
// 然后 `go run ./cmd/contract-gen -w`。手改本文件会被门禁判陈旧。

export interface QARequest {
  question: string;
  session: string;
}

export interface QAResponse {
  question: string;
  answer: string;
  refused: boolean;
  refusal_reason?: string;
  citations: string[];
  route: RouteView;
  escalation: EscalationRecord;
  reuse: ReuseState;
  coverage: CoverageView;
  eviction: EvictionView;
  rerank: RerankState;
  windows: WindowView[];
  usage: UsageView;
}

export interface RouteView {
  action: string;
  reason: string;
  signals: RouteSignals;
}

export interface RouteSignals {
  coverage: number;
  margin: number;
  dead_rate: number;
  windows: number;
  confidence: number;
}

export interface EscalationRecord {
  triggered: boolean;
  executed: boolean;
  before: string;
  after: string;
  windows: number;
  reason?: string;
}

export interface ReuseState {
  hit: boolean;
  reason?: string;
}

export interface CoverageView {
  value: number;
  out_of_corpus_terms?: string[];
}

export interface EvictionView {
  merged: number;
  dropped: number;
}

export interface RerankState {
  applied: boolean;
  reason?: string;
}

export interface WindowView {
  source_id: string;
  title: string;
  span: string;
  text: string;
  score: number;
}

export interface UsageView {
  prompt_tokens: number;
  completion_tokens: number;
  cost_known: boolean;
}

export interface HealthResponse {
  status: string;
  corpus_docs: number;
  realm: string;
}

export interface StatusResponse {
  synthesis: boolean;
  embedder: boolean;
  reuse: boolean;
  escalate: boolean;
}

export interface IngestRequest {
  body: string;
  url: string;
}

export interface IngestResponse {
  id: string;
  corpus_docs: number;
  bytes: number;
}

export interface DocResponse {
  id: string;
  body: string;
  span_start: number;
  span_end: number;
}


