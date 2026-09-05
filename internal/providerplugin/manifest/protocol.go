package manifest

type Operation struct {
	ID                string              `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9_-]*$,maxLength=64"`
	Kind              string              `json:"kind" jsonschema:"required,enum=inference,enum=model-catalog,enum=account,enum=usage,enum=compaction,enum=custom"`
	Protocol          string              `json:"protocol" jsonschema:"required,enum=anthropic-messages,enum=openai-responses,enum=gemini-generate-content,enum=gemini-interactions,enum=generic-json"`
	Transport         string              `json:"transport" jsonschema:"required,enum=http-json,enum=sse,enum=websocket-json"`
	Endpoint          string              `json:"endpoint" jsonschema:"required,maxLength=64"`
	Method            string              `json:"method,omitempty" jsonschema:"enum=GET,enum=POST,enum=PUT,enum=PATCH,enum=DELETE"`
	Path              string              `json:"path" jsonschema:"required,pattern=^/,maxLength=1024"`
	ClientIdentity    string              `json:"client_identity,omitempty" jsonschema:"maxLength=64"`
	Headers           []HeaderRule        `json:"headers,omitempty" jsonschema:"maxItems=128"`
	RequestTransform  string              `json:"request_transform,omitempty" jsonschema:"maxLength=64"`
	ResponseTransform string              `json:"response_transform,omitempty" jsonschema:"maxLength=64"`
	PromptTransform   string              `json:"prompt_transform,omitempty" jsonschema:"maxLength=64"`
	RoleMap           string              `json:"role_map,omitempty" jsonschema:"maxLength=64"`
	ToolCodec         string              `json:"tool_codec,omitempty" jsonschema:"maxLength=64"`
	Streaming         *StreamingPolicy    `json:"streaming,omitempty"`
	Retry             *RetryPolicy        `json:"retry,omitempty"`
	Continuation      *ContinuationPolicy `json:"continuation,omitempty"`
	Compaction        *CompactionPolicy   `json:"compaction,omitempty"`
	Timeouts          *TimeoutHints       `json:"timeouts,omitempty"`
}

type StreamingPolicy struct {
	EventSource      string         `json:"event_source" jsonschema:"required,enum=sse-data-json,enum=websocket-json,enum=json-sequence"`
	DoneMarker       string         `json:"done_marker,omitempty" jsonschema:"maxLength=128"`
	EventTypePointer string         `json:"event_type_pointer,omitempty" jsonschema:"pattern=^/"`
	Mappings         []EventMapping `json:"mappings" jsonschema:"required,minItems=1,maxItems=256"`
	RequireTerminal  bool           `json:"require_terminal" jsonschema:"required"`
	UnknownEvent     string         `json:"unknown_event" jsonschema:"required,enum=ignore,enum=warn,enum=error"`
	MaxEventBytes    int64          `json:"max_event_bytes,omitempty" jsonschema:"minimum=1,maximum=16777216"`
}

type EventMapping struct {
	Source            string            `json:"source" jsonschema:"required,maxLength=256"`
	Event             string            `json:"event" jsonschema:"required,enum=warning,enum=text-start,enum=text-delta,enum=text-end,enum=reasoning-start,enum=reasoning-delta,enum=reasoning-end,enum=tool-input-start,enum=tool-input-delta,enum=tool-input-end,enum=tool-call,enum=tool-result,enum=source,enum=usage,enum=finish,enum=error"`
	Fields            map[string]string `json:"fields,omitempty" jsonschema:"description=Normalized field name to source JSON Pointer"`
	MetadataNamespace string            `json:"metadata_namespace,omitempty" jsonschema:"maxLength=128"`
	Condition         *Predicate        `json:"condition,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts       int      `json:"max_attempts" jsonschema:"required,minimum=1,maximum=10"`
	InitialDelayMS    int      `json:"initial_delay_ms,omitempty" jsonschema:"minimum=0,maximum=60000"`
	MaxDelayMS        int      `json:"max_delay_ms,omitempty" jsonschema:"minimum=0,maximum=60000"`
	Factor            float64  `json:"factor,omitempty" jsonschema:"minimum=1,maximum=10"`
	Jitter            bool     `json:"jitter,omitempty"`
	RetryAfter        bool     `json:"retry_after,omitempty"`
	Statuses          []int    `json:"statuses,omitempty" jsonschema:"uniqueItems=true,maxItems=64"`
	Codes             []string `json:"codes,omitempty" jsonschema:"uniqueItems=true,maxItems=128"`
	TransportErrors   bool     `json:"transport_errors,omitempty"`
	UnexpectedEOF     bool     `json:"unexpected_eof,omitempty"`
	Authentication    string   `json:"authentication" jsonschema:"required,enum=never,enum=refresh-once"`
	ReplayRequirement string   `json:"replay_requirement" jsonschema:"required,enum=idempotent,enum=before-first-event,enum=never"`
}

type ContinuationPolicy struct {
	Mode                 string   `json:"mode" jsonschema:"required,enum=none,enum=previous-response,enum=previous-interaction"`
	ResponseIDPointer    string   `json:"response_id_pointer,omitempty" jsonschema:"pattern=^/"`
	RequestField         string   `json:"request_field,omitempty" jsonschema:"maxLength=128"`
	RequiredStableFields []string `json:"required_stable_fields,omitempty" jsonschema:"uniqueItems=true,maxItems=64"`
	AppendOnlyHistory    bool     `json:"append_only_history,omitempty"`
	Store                string   `json:"store" jsonschema:"required,enum=required,enum=optional,enum=forbidden"`
	Fallback             string   `json:"fallback" jsonschema:"required,enum=full-replay,enum=error"`
	MaxIdleSeconds       int64    `json:"max_idle_seconds,omitempty" jsonschema:"minimum=1,maximum=86400"`
	MetadataNamespace    string   `json:"metadata_namespace,omitempty" jsonschema:"maxLength=128"`
}

type CompactionPolicy struct {
	Mode                string `json:"mode" jsonschema:"required,enum=none,enum=local-summary,enum=remote-operation"`
	Operation           string `json:"operation,omitempty" jsonschema:"maxLength=64"`
	RetainedTokenBudget int64  `json:"retained_token_budget,omitempty" jsonschema:"minimum=1"`
	PreserveToolPairs   bool   `json:"preserve_tool_pairs,omitempty"`
	MetadataNamespace   string `json:"metadata_namespace,omitempty" jsonschema:"maxLength=128"`
}

type TimeoutHints struct {
	ConnectSeconds int `json:"connect_seconds,omitempty" jsonschema:"minimum=1,maximum=120"`
	RequestSeconds int `json:"request_seconds,omitempty" jsonschema:"minimum=1,maximum=600"`
	IdleSeconds    int `json:"idle_seconds,omitempty" jsonschema:"minimum=1,maximum=600"`
}

type UsagePolicy struct {
	Setup        []UsageSetup   `json:"setup,omitempty" jsonschema:"maxItems=8"`
	Operation    string         `json:"operation,omitempty" jsonschema:"maxLength=64"`
	Source       string         `json:"source" jsonschema:"required,enum=response,enum=stream,enum=operation"`
	Mappings     []UsageMapping `json:"mappings,omitempty" jsonschema:"maxItems=64"`
	Windows      []WindowMap    `json:"windows,omitempty" jsonschema:"maxItems=32"`
	PlanPointers []string       `json:"plan_pointers,omitempty" jsonschema:"maxItems=8,pattern=^/,maxLength=1024"`
	Fallback     string         `json:"fallback" jsonschema:"required,enum=zero,enum=estimate,enum=unavailable"`
}

type UsageSetup struct {
	Operation    string                   `json:"operation" jsonschema:"required,maxLength=64"`
	Extract      []UsageContextExtraction `json:"extract,omitempty" jsonschema:"maxItems=16"`
	PlanPointers []string                 `json:"plan_pointers,omitempty" jsonschema:"maxItems=8,pattern=^/,maxLength=1024"`
}

type UsageContextExtraction struct {
	Context string `json:"context" jsonschema:"required,pattern=^[a-z][a-z0-9_.-]*$,maxLength=128"`
	Pointer string `json:"pointer" jsonschema:"required,pattern=^/,maxLength=1024"`
}

type UsageMapping struct {
	Target    string `json:"target" jsonschema:"required,enum=input_tokens,enum=output_tokens,enum=reasoning_tokens,enum=cache_read_tokens,enum=cache_write_tokens,enum=total_tokens"`
	Pointer   string `json:"pointer" jsonschema:"required,pattern=^/"`
	Operation string `json:"operation,omitempty" jsonschema:"enum=copy,enum=subtract-cache-read,enum=accumulate,enum=replace"`
}

type WindowMap struct {
	ID                       string `json:"id" jsonschema:"required,minLength=1,maxLength=64"`
	UsedPointer              string `json:"used_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	LimitPointer             string `json:"limit_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	RemainingPointer         string `json:"remaining_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	RemainingFractionPointer string `json:"remaining_fraction_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	ResetPointer             string `json:"reset_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	ResetFormat              string `json:"reset_format,omitempty" jsonschema:"enum=unix-seconds,enum=unix-milliseconds,enum=rfc3339,enum=duration-seconds"`
}

type ImagePolicy struct {
	AcceptedMediaTypes []string            `json:"accepted_media_types" jsonschema:"required,minItems=1,uniqueItems=true,maxItems=32"`
	MaxSourceBytes     int64               `json:"max_source_bytes" jsonschema:"required,minimum=1,maximum=104857600"`
	MaxSidePixels      int                 `json:"max_side_pixels,omitempty" jsonschema:"minimum=1,maximum=32768"`
	MaxOutputBytes     int64               `json:"max_output_bytes,omitempty" jsonschema:"minimum=1,maximum=104857600"`
	MaxPatches         int                 `json:"max_patches,omitempty" jsonschema:"minimum=1"`
	OutputMediaType    string              `json:"output_media_type,omitempty" jsonschema:"maxLength=128"`
	FlattenAlpha       string              `json:"flatten_alpha,omitempty" jsonschema:"enum=none,enum=white,enum=black"`
	QualitySteps       []int               `json:"quality_steps,omitempty" jsonschema:"uniqueItems=true,maxItems=32"`
	ResizePercent      int                 `json:"resize_percent,omitempty" jsonschema:"minimum=1,maximum=99"`
	HistoryBudget      *ImageHistoryBudget `json:"history_budget,omitempty"`
}

type ImageHistoryBudget struct {
	RequestBytes      int64   `json:"request_bytes" jsonschema:"required,minimum=1"`
	RetryRequestBytes int64   `json:"retry_request_bytes,omitempty" jsonschema:"minimum=1"`
	PerImageTargets   []int64 `json:"per_image_targets,omitempty" jsonschema:"minimum=1,minItems=1,uniqueItems=true,maxItems=16"`
	OmitOldImages     bool    `json:"omit_old_images,omitempty"`
	RetainNewestImage bool    `json:"retain_newest_image,omitempty"`
}

type InstructionPolicy struct {
	Profiles         map[string]string `json:"profiles" jsonschema:"required,minProperties=1,maxProperties=32,description=Profile ID to bundle-relative UTF-8 text file"`
	Default          string            `json:"default" jsonschema:"required,maxLength=64"`
	SelectionDefault string            `json:"selection_default,omitempty" jsonschema:"enum=crux,enum=native"`
	HiddenSkills     []string          `json:"hidden_skills,omitempty" jsonschema:"uniqueItems=true,maxItems=64"`
}

type RuntimeControl struct {
	ID          string   `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9_.-]*$,maxLength=64"`
	Label       string   `json:"label" jsonschema:"required,maxLength=128"`
	Description string   `json:"description,omitempty" jsonschema:"maxLength=1024"`
	Type        string   `json:"type" jsonschema:"required,enum=boolean,enum=integer,enum=number,enum=string,enum=enum"`
	Values      []string `json:"values,omitempty" jsonschema:"uniqueItems=true,maxItems=64"`
	Default     any      `json:"default,omitempty"`
	Scope       string   `json:"scope" jsonschema:"required,enum=provider,enum=model,enum=request"`
	RequestPath string   `json:"request_path,omitempty" jsonschema:"pattern=^/"`
}

type MetadataContract struct {
	Namespace         string         `json:"namespace" jsonschema:"required,pattern=^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$,maxLength=128"`
	Version           int            `json:"version" jsonschema:"required,minimum=1"`
	Scope             string         `json:"scope" jsonschema:"required,enum=message,enum=reasoning,enum=text,enum=tool-call,enum=tool-result,enum=continuation,enum=compaction"`
	Schema            map[string]any `json:"schema" jsonschema:"required"`
	RequiredForReplay bool           `json:"required_for_replay,omitempty"`
	LegacyProjection  string         `json:"legacy_projection,omitempty" jsonschema:"maxLength=128"`
}

type ErrorMapping struct {
	Class           string   `json:"class" jsonschema:"required,enum=authentication,enum=authorization,enum=rate-limit,enum=capacity,enum=context-overflow,enum=invalid-request,enum=content-filter,enum=server,enum=transport,enum=unknown"`
	Statuses        []int    `json:"statuses,omitempty" jsonschema:"minimum=100,maximum=599,uniqueItems=true,maxItems=64"`
	Codes           []string `json:"codes,omitempty" jsonschema:"minLength=1,maxLength=256,uniqueItems=true,maxItems=128"`
	CodePointer     string   `json:"code_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	MessagePointer  string   `json:"message_pointer,omitempty" jsonschema:"pattern=^/,maxLength=1024"`
	Title           string   `json:"title,omitempty" jsonschema:"maxLength=128"`
	Retryable       bool     `json:"retryable,omitempty"`
	ContextOverflow bool     `json:"context_overflow,omitempty"`
}
