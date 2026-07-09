// Mirrors docs/openapi.yaml (Agent Runtime API 0.1.0).
// For a generated source of truth, run:
//   npx openapi-typescript ../../docs/openapi.yaml -o src/lib/api.gen.ts
// and re-export from here. Kept hand-written for now to avoid a build step.

// JSON-serializable value. Server functions cross the network boundary, and
// TanStack Start rejects `unknown` in their return types — use this instead.
export type JsonValue =
  | string
  | number
  | boolean
  | null
  | Array<JsonValue>
  | { [key: string]: JsonValue }

export type RunStatus =
  | 'queued'
  | 'running'
  | 'paused'
  | 'completed'
  | 'failed'
  | 'cancelled'

export type RuntimeKind = 'native_sdk' | 'codex' | 'opencode'

export interface TargetRef {
  type: string
  id: string
  display?: { title?: string; url?: string }
  metadata?: Record<string, JsonValue>
}

export interface WorkspaceLease {
  id: string
  provider?: string
  root_path: string
  cleanup_policy?: 'always' | 'on_terminal' | 'manual'
  metadata?: Record<string, JsonValue>
}

export interface Agent {
  id?: string
  app_id: string
  name: string
  runtime_kind?: RuntimeKind
  provider?: string
  model?: string
  system_prompt?: string
  allowed_tools?: Array<string>
  allowed_targets?: Array<string>
  approval_mode?: 'never' | 'always'
  default_invocation_mode?: 'autonomous' | 'interactive'
  execution_config?: Record<string, JsonValue>
}

export interface AgentRun {
  id: string
  app_id: string
  host_run_id?: string
  agent_id: string
  target: TargetRef
  runtime_kind: string
  execution_mode: string
  invocation_mode: string
  external_actor_id?: string
  status: RunStatus
  pause_reason?: string
  approval_state?: string
  input?: Record<string, JsonValue>
  output_summary?: Record<string, JsonValue>
  workspace_lease?: WorkspaceLease
  error_message?: string
  started_at?: string
  completed_at?: string
  created_at?: string
  updated_at?: string
}

export interface StartRunRequest {
  app_id: string
  host_run_id?: string
  agent_id: string
  target: TargetRef
  instructions?: string
  allowed_tools?: Array<string>
  external_actor_id?: string
  mode?: 'autonomous' | 'interactive'
  execution_mode?: 'lightweight' | 'durable'
  trigger?: Record<string, JsonValue>
  metadata?: Record<string, JsonValue>
  turn_policy?: Record<string, JsonValue>
}

export interface Message {
  id?: string
  app_id?: string
  run_id?: string
  role?: string
  content?: string
  message_type?: string
  sequence_no?: number
  runtime_message_id?: string
  content_blocks?: JsonValue
  tool_invocations?: JsonValue
  created_at?: string
}

export interface Artifact {
  id?: string
  app_id?: string
  run_id?: string
  artifact_type?: string
  format?: string
  storage_mode?: string
  inline_content?: string
  metadata?: Record<string, JsonValue>
  sequence_no?: number
  created_at?: string
}

export interface Interaction {
  id?: string
  app_id?: string
  run_id?: string
  runtime_kind?: string
  interaction_kind?: string
  status?: string
  title?: string
  summary?: string
  request_payload?: Record<string, JsonValue>
  response_payload?: Record<string, JsonValue>
  resolved_by_external_id?: string
  resolved_at?: string
  created_at?: string
  updated_at?: string
}

export interface ToolCall {
  id?: string
  app_id?: string
  run_id?: string
  tool_name?: string
  input?: Record<string, JsonValue>
  output?: Record<string, JsonValue>
  error?: string
  mutating?: boolean
  approval_required?: boolean
  created_at?: string
}

export interface ResumeRunRequest {
  intent: 'approve' | 'request_changes' | 'reply' | 'auth_completed'
  content?: string
  response_payload?: Record<string, JsonValue>
  external_actor_id?: string
  resume_id?: string
  interaction_id?: string
}

export interface ToolDefinition {
  name: string
  description?: string
  category?: string
  input_schema?: JsonValue
  mutating?: boolean
  supported_target_types?: Array<string>
}

export interface ProviderCapability {
  name: string
  configured: boolean
  default_model?: string
  base_url_overridden?: boolean
}

export interface SkillInfo {
  key: string
  title?: string
  description?: string
}

export interface AppComponent {
  name: string
  kind: string
  configured: boolean
  url?: string
  transport?: string
  auth_configured: boolean
}

export interface AppSummary {
  app_id: string
  components: Array<AppComponent>
}

export interface AppComponentHealth extends AppComponent {
  status: 'configured' | 'reachable' | 'error'
  http_status?: number
  error?: string
}

export interface AppHealth {
  app_id: string
  components: Array<AppComponentHealth>
  error?: string
}

export interface RunEvent {
  event_id: string
  sent_at: string
  sequence_no: number
  app_id: string
  run_id: string
  host_run_id?: string
  type: string
  data?: Record<string, JsonValue>
}

export interface RunExecutionInfo {
  execution_mode: string
  state: string
  error?: string
  workflow_id?: string
  temporal_run_id?: string
  task_queue?: string
  history_length?: number
  history_size_bytes?: number
  state_transition_count?: number
  started_at?: string
  closed_at?: string
}

export interface RunPage {
  items: Array<AgentRun>
  total: number
  limit: number
  offset: number
}

export interface Capabilities {
  runtime_kinds: Array<string>
  providers: Array<ProviderCapability>
  store: { driver: string; in_memory: boolean }
  durable: { enabled: boolean; temporal_address?: string; namespace?: string }
  skills?: Array<SkillInfo>
  apps?: Array<AppSummary>
  service_auth_enabled: boolean
  tools: Array<ToolDefinition>
}

export const ACTIVE_RUN_STATUSES: ReadonlyArray<RunStatus> = [
  'queued',
  'running',
  'paused',
]

export function isActiveRun(status: RunStatus): boolean {
  return ACTIVE_RUN_STATUSES.includes(status)
}
