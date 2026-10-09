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
  analysis: AnalysisView;
  prior?: PriorView[];
  facts?: FactView[];
  conflicts?: ConflictView[];
  abstain?: AbstainView;
  route: RouteView;
  escalation: EscalationRecord;
  reuse: ReuseState;
  coverage: CoverageView;
  eviction: EvictionView;
  rerank: RerankState;
  windows: WindowView[];
  usage: UsageView;
  classification?: ClassificationView;
  committed: CommittedView;
}

export interface AnalysisView {
  intent: string;
  primary: Record<string, unknown>;
  out_of_corpus_terms?: string[];
  score: number;
}

export interface PriorView {
  doc_id: string;
  score: number;
  signals: Record<string, unknown>;
  title?: string;
}

export interface FactView {
  id: string;
  query: string;
  covered: boolean;
  near_miss?: number;
  supports?: SupportView[];
  judge?: string;
}

export interface SupportView {
  source_id: string;
  title?: string;
  span: string;
  score: number;
}

export interface ConflictView {
  fact_id: string;
  values: string[];
  source_ids: string[];
}

export interface AbstainView {
  p_fail: number;
  action: string;
  reason?: string;
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
  tier: string;
  confidence_known: boolean;
  calibration_program: string;
  threshold_version?: string;
  threshold: number;
  gap_thin: boolean;
  facts_k: number;
  facts_covered: number;
  facts_missing: number;
  conflicts: number;
}

export interface EscalationRecord {
  triggered: boolean;
  executed: boolean;
  before: string;
  after: string;
  windows: number;
  reason?: string;
  bridge?: string;
  bridge_terms?: string[];
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

export interface ClassificationView {
  enabled: boolean;
  applied: boolean;
  outcome?: string;
  counts?: Record<string, unknown>;
}

export interface CommittedView {
  at: Record<string, unknown>;
  realm: string;
  flow: string;
  corpus_version: string;
  config_version: string;
  strategy_version: string;
  belief_version: string;
  calibration: Calibration;
}

export interface Time {
}

export interface Location {
}

export interface zone {
}

export interface zoneTrans {
}

export interface Calibration {
  tier: string;
  program: string;
  threshold: number;
  threshold_version: string;
}

export interface HealthResponse {
  status: string;
  corpus_docs: number;
  realm: string;
  corpus_version: string;
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
  encoding?: string;
  src_digest?: string;
  src_bytes?: number;
}


