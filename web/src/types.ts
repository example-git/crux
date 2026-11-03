export type Owner = { provider_id: string; has_oauth?: boolean; [key: string]: unknown };
export type ProviderUsage = { provider_id: string; plan?: string; windows?: { name: string; percent: number; resets_at?: string }[]; fetched_at?: string };
export type UsageSnapshot = { connection: string; workspaceID: string; owner: Owner; revision: number; digest: string; usage?: ProviderUsage };
export type Model = {
  id: string; name: string; context_window: number; default_max_tokens: number;
  can_reason?: boolean; reasoning_levels?: string[]; default_reasoning_effort?: string; supports_attachments?: boolean;
  cost_per_1m_in?: number; cost_per_1m_out?: number;
};
export type SelectedModel = { provider: string; model: string; max_tokens?: number; reasoning_effort?: string; think?: boolean; [key: string]: unknown };
export type ModelSlot = { model: SelectedModel; owner: Owner };
export type Models = { large?: ModelSlot; small?: ModelSlot };
export type RuntimeControl = {
  id: string; label: string; description?: string; type: 'boolean' | 'integer' | 'number' | 'string' | 'enum';
  values?: string[]; available: boolean; available_models?: string[]; descriptor_digest?: string; diagnostic?: string; [key: string]: unknown;
};
export type Authentication = { kind: string; flow_id?: string; adapter?: 'browser' | 'hosted-paste' | 'device-code'; available: boolean; diagnostic?: string };
export type Provider = {
  id: string; name: string; owner?: Owner; available: boolean; availability?: string; diagnostic?: string; description?: string;
  models?: Model[]; default_large_model?: string; default_small_model?: string; flat_rate?: boolean;
  brand?: { label?: string; short_name?: string; color?: string; gradient_a?: string; gradient_b?: string };
  authentication?: Authentication[];
  instructions?: { default: string; selection_default?: string; profiles: Record<string, string> };
  runtime_controls?: RuntimeControl[]; usage_available?: boolean;
};
export type Workspace = {
  id: string; path: string; data_dir: string; requested_data_dir?: string; connection_name?: string; yolo?: boolean;
  config: { models: Record<string, SelectedModel>; recent_models?: Record<string, { provider: string; model: string }[]>; options: Record<string, any>; providers?: Record<string, any>; mcp?: Record<string, any>; tools?: Record<string, any>; [key: string]: any };
  provider_surfaces?: Provider[]; provider_load_issues?: Record<string, any>[];
  authority?: { mode: string; revision?: number; digest?: string };
};
export type Goal = { objective?: string; status?: string; generation?: number; error?: string; updated_at?: number };
export type SessionInstructions = { mode: 'inherit' | 'append' | 'replace'; text: string; revision: number };
export type Session = {
  id: string; title: string; archived: boolean; parent_session_id?: string; forked_from_session_id?: string; mode: string;
  plan?: string; goal?: Goal; analysis_effort?: string; estimated_usage?: boolean; unseen_local_tokens?: number; summary_message_id?: string;
  message_count: number; prompt_tokens: number; completion_tokens: number; cost: number;
  created_at: number; updated_at: number; is_busy: boolean; attached_clients: number;
  model?: string; provider?: string; models?: Models; model_revision?: number; instructions?: SessionInstructions;
  todos?: { content: string; status: string; active_form?: string }[];
};
export type Part = { type: string; data: Record<string, any> };
export type Message = { id: string; session_id: string; role: string; parts: Part[]; created_at: number; updated_at: number; model: string; provider: string; is_summary_message?: boolean };
export type Agent = { is_busy: boolean; is_ready: boolean; model: Model; model_cfg: SelectedModel };
export type Permission = { id: string; session_id: string; tool_call_id: string; tool_name: string; description: string; action: string; path: string; params: unknown };
export type Question = {
  id: string; session_id: string; confirm_title?: string; confirm_description?: string;
  questions: { id: string; type: string; label?: string; question: string; description: string; choices?: { id: string; label: string; description?: string }[] }[];
};
export type Startup = { path: string; data_dir?: string; debug: boolean; yolo: boolean; channels?: string[]; session_id?: string; continue: boolean; version: string; settings_path: string };
export type Event = { type: string; payload: { type: string; payload: any } };

export type DeliveryMode = 'queue' | 'steer';
export type QueuedPrompt = { delivery_mode: DeliveryMode; submission_id: string; prompt: string };
export type InputPart = { type: 'skill' | 'text'; text: string; path?: string };
export type Attachment = { file_path?: string; file_name: string; mime_type: string; content: string };
export type Checkpoint = { id: string; session_id: string; path: string; content: string; exists: boolean; version: number; created_at: number };
export type Task = {
  id: string; type: string; description: string; command?: string; child_session_id?: string; agent_type?: string;
  ownership: { parent_session_id: string }; state: { status: string; started_at?: string; ended_at?: string; exit_code?: number; error_message?: string; lost_reason?: string; stop_requested_at?: string };
  working_directory?: string; output_ref?: string; continuation_of?: string; usage?: { prompt_tokens: number; completion_tokens: number; cost: number; tool_use_count: number };
  final_output?: string;
};
export type Skill = { id: string; name: string; label?: string; description: string; source: string; user_invocable: boolean };
export type FileMatch = { root: string; path: string; file_name: string; match_type?: string; score?: number; indices?: number[] };
export type ProjectInfo = { slug: string; name: string; status: string; selected: boolean; completed: number; total: number };
export type MCPPrompt = { id: string; prompt_id: string; client_id: string; title?: string; description?: string; arguments?: { id: string; title: string; description?: string; required: boolean }[] };
