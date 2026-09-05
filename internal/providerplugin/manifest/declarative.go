package manifest

// Credential declares a host-owned secret or OAuth credential. The host stores
// values and exposes only opaque handles to extensions.
type Credential struct {
	ID             string   `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9_-]*$,maxLength=64"`
	Kind           string   `json:"kind" jsonschema:"required,enum=api-key,enum=bearer,enum=oauth2,enum=none"`
	ConfigProperty string   `json:"config_property,omitempty" jsonschema:"maxLength=128"`
	Audience       []string `json:"audience,omitempty" jsonschema:"uniqueItems=true,maxItems=32,description=Endpoint IDs to which this credential may be attached"`
	Scopes         []string `json:"scopes,omitempty" jsonschema:"uniqueItems=true,maxItems=128"`
	LegacyFields   []string `json:"legacy_fields,omitempty" jsonschema:"uniqueItems=true,maxItems=32"`
}

type OAuthFlow struct {
	ID                    string          `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9_-]*$,maxLength=64"`
	Credential            string          `json:"credential" jsonschema:"required,maxLength=64"`
	AuthorizationEndpoint string          `json:"authorization_endpoint" jsonschema:"required,maxLength=64"`
	TokenEndpoint         string          `json:"token_endpoint" jsonschema:"required,maxLength=64"`
	RevocationEndpoint    string          `json:"revocation_endpoint,omitempty" jsonschema:"maxLength=64"`
	ClientID              Template        `json:"client_id" jsonschema:"required"`
	ClientSecret          *Template       `json:"client_secret,omitempty"`
	Scopes                []string        `json:"scopes" jsonschema:"required,minItems=1,uniqueItems=true,maxItems=128"`
	RefreshScopes         []string        `json:"refresh_scopes,omitempty" jsonschema:"uniqueItems=true,maxItems=128"`
	PKCE                  string          `json:"pkce" jsonschema:"required,enum=required-s256,enum=optional-s256,enum=disabled"`
	Redirect              OAuthRedirect   `json:"redirect" jsonschema:"required"`
	AuthorizationParams   []QueryRule     `json:"authorization_params,omitempty" jsonschema:"maxItems=64"`
	TokenRequest          TokenRequest    `json:"token_request" jsonschema:"required"`
	TokenResponse         TokenResponse   `json:"token_response" jsonschema:"required"`
	DeviceCode            *DeviceCodeFlow `json:"device_code,omitempty"`
	TimeoutSeconds        int             `json:"timeout_seconds,omitempty" jsonschema:"minimum=1,maximum=600"`
}

type DeviceCodeFlow struct {
	Endpoint               string       `json:"endpoint" jsonschema:"required,maxLength=64"`
	Request                []FieldRule  `json:"request" jsonschema:"required,minItems=1,maxItems=64"`
	Headers                []HeaderRule `json:"headers,omitempty" jsonschema:"maxItems=32"`
	DeviceCodePointer      string       `json:"device_code_pointer" jsonschema:"required,pattern=^/"`
	UserCodePointer        string       `json:"user_code_pointer" jsonschema:"required,pattern=^/"`
	VerificationURLPointer string       `json:"verification_url_pointer" jsonschema:"required,pattern=^/"`
	ExpiresInPointer       string       `json:"expires_in_pointer,omitempty" jsonschema:"pattern=^/"`
	IntervalPointer        string       `json:"interval_pointer,omitempty" jsonschema:"pattern=^/"`
	DefaultIntervalSeconds int          `json:"default_interval_seconds" jsonschema:"required,minimum=1,maximum=60"`
	Poll                   []FieldRule  `json:"poll" jsonschema:"required,minItems=1,maxItems=64"`
	ErrorPointer           string       `json:"error_pointer" jsonschema:"required,pattern=^/"`
	MaxBodyBytes           int64        `json:"max_body_bytes" jsonschema:"required,minimum=1,maximum=10485760"`
}

type OAuthRedirect struct {
	Mode          string `json:"mode" jsonschema:"required,enum=loopback-dynamic,enum=loopback-fixed,enum=hosted-paste,enum=device-code"`
	URI           string `json:"uri,omitempty" jsonschema:"format=uri,maxLength=2048"`
	CallbackPath  string `json:"callback_path,omitempty" jsonschema:"pattern=^/,maxLength=256"`
	Port          int    `json:"port,omitempty" jsonschema:"minimum=1,maximum=65535"`
	StateRequired bool   `json:"state_required" jsonschema:"required"`
}

type QueryRule struct {
	Name  string   `json:"name" jsonschema:"required,maxLength=128"`
	Value Template `json:"value" jsonschema:"required"`
}

type TokenRequest struct {
	Encoding  string       `json:"encoding" jsonschema:"required,enum=form,enum=json"`
	AuthStyle string       `json:"auth_style" jsonschema:"required,enum=params,enum=basic,enum=none"`
	Code      []FieldRule  `json:"authorization_code" jsonschema:"required,minItems=1,maxItems=64"`
	Refresh   []FieldRule  `json:"refresh_token" jsonschema:"required,minItems=1,maxItems=64"`
	Headers   []HeaderRule `json:"headers,omitempty" jsonschema:"maxItems=32"`
}

type TokenResponse struct {
	AccessTokenPointer   string `json:"access_token_pointer" jsonschema:"required,pattern=^/"`
	RefreshTokenPointer  string `json:"refresh_token_pointer,omitempty" jsonschema:"pattern=^/"`
	ExpiresInPointer     string `json:"expires_in_pointer,omitempty" jsonschema:"pattern=^/"`
	TokenTypePointer     string `json:"token_type_pointer,omitempty" jsonschema:"pattern=^/"`
	DefaultExpiresIn     int64  `json:"default_expires_in_seconds,omitempty" jsonschema:"minimum=1,maximum=31536000"`
	PreserveRefreshToken bool   `json:"preserve_refresh_token_when_omitted" jsonschema:"required"`
	MaxBodyBytes         int64  `json:"max_body_bytes,omitempty" jsonschema:"minimum=1,maximum=10485760"`
}

type Endpoint struct {
	ID              string   `json:"id" jsonschema:"required,pattern=^[a-z][a-z0-9_-]*$,maxLength=64"`
	BaseURL         string   `json:"base_url" jsonschema:"required,format=uri,maxLength=2048"`
	AllowedSchemes  []string `json:"allowed_schemes" jsonschema:"required,minItems=1,uniqueItems=true,enum=https,enum=wss"`
	AllowedHosts    []string `json:"allowed_hosts" jsonschema:"required,minItems=1,uniqueItems=true,maxItems=32"`
	Override        string   `json:"override" jsonschema:"required,enum=forbidden,enum=same-origin,enum=allowed-hosts"`
	Credential      string   `json:"credential,omitempty" jsonschema:"maxLength=64"`
	FollowRedirects bool     `json:"follow_redirects,omitempty"`
}

type HeaderRule struct {
	Operation string    `json:"operation" jsonschema:"required,enum=set,enum=set-if-absent,enum=append,enum=append-unique,enum=delete"`
	Name      string    `json:"name" jsonschema:"required,pattern=^[!#$%&'*+.^_|~0-9A-Za-z-]+$,maxLength=128"`
	Value     *Template `json:"value,omitempty"`
	Protected bool      `json:"protected,omitempty" jsonschema:"description=Prevents user and extension overrides after this rule"`
}

// Template is a finite, non-executable value expression. Exactly the fields
// allowed by Kind are populated; semantic validation enforces that union.
type Template struct {
	Kind  string     `json:"kind" jsonschema:"required,enum=literal,enum=config,enum=credential,enum=context,enum=concat,enum=uuid,enum=unix-time,enum=random-hex"`
	Value any        `json:"value,omitempty" jsonschema:"description=JSON literal value; used only when kind is literal"`
	Ref   string     `json:"ref,omitempty" jsonschema:"maxLength=256"`
	Parts []Template `json:"parts,omitempty" jsonschema:"maxItems=64"`
	Bytes int        `json:"bytes,omitempty" jsonschema:"minimum=1,maximum=64"`
}

type FieldRule struct {
	Name      string   `json:"name" jsonschema:"required,maxLength=128"`
	Value     Template `json:"value" jsonschema:"required"`
	OmitEmpty bool     `json:"omit_empty,omitempty"`
}

type JSONPipeline struct {
	MaxOperations int             `json:"max_operations,omitempty" jsonschema:"minimum=1,maximum=256"`
	Operations    []JSONOperation `json:"operations" jsonschema:"required,minItems=1,maxItems=256"`
}

type JSONOperation struct {
	Operation string     `json:"operation" jsonschema:"required,enum=set,enum=set-if-absent,enum=delete,enum=copy,enum=move,enum=rename-key,enum=filter-array,enum=keep-keys,enum=drop-keys"`
	Path      string     `json:"path" jsonschema:"required,pattern=^(|/)"`
	From      string     `json:"from,omitempty" jsonschema:"pattern=^/"`
	Value     *Template  `json:"value,omitempty"`
	Keys      []string   `json:"keys,omitempty" jsonschema:"uniqueItems=true,maxItems=128"`
	Predicate *Predicate `json:"predicate,omitempty"`
}

type Predicate struct {
	Operation string     `json:"operation" jsonschema:"required,enum=exists,enum=equals,enum=not-equals,enum=contains,enum=starts-with,enum=matches-enum"`
	Path      string     `json:"path,omitempty" jsonschema:"pattern=^(|/)"`
	Value     *Template  `json:"value,omitempty"`
	Values    []Template `json:"values,omitempty" jsonschema:"maxItems=64"`
}

type PromptPipeline struct {
	Operations []PromptOperation `json:"operations" jsonschema:"required,minItems=1,maxItems=128"`
}

type PromptOperation struct {
	Operation string     `json:"operation" jsonschema:"required,enum=prepend,enum=append,enum=insert-after-role,enum=remove-lines-with-prefix,enum=drop-role,enum=join-adjacent-role"`
	Role      string     `json:"role,omitempty" jsonschema:"enum=system,enum=developer,enum=user,enum=assistant,enum=tool"`
	Text      *Template  `json:"text,omitempty"`
	Prefix    string     `json:"prefix,omitempty" jsonschema:"maxLength=1024"`
	When      *Predicate `json:"when,omitempty"`
}

type RoleMap struct {
	System    string `json:"system" jsonschema:"required,maxLength=64"`
	Developer string `json:"developer,omitempty" jsonschema:"maxLength=64"`
	User      string `json:"user" jsonschema:"required,maxLength=64"`
	Assistant string `json:"assistant" jsonschema:"required,maxLength=64"`
	Tool      string `json:"tool" jsonschema:"required,maxLength=64"`
	Unknown   string `json:"unknown" jsonschema:"required,enum=reject,enum=drop,enum=warn-drop"`
}

type ToolCodec struct {
	Aliases         []ToolAlias       `json:"aliases,omitempty" jsonschema:"maxItems=512"`
	PrefixAliases   []ToolPrefixAlias `json:"prefix_aliases,omitempty" jsonschema:"maxItems=64"`
	Parameters      []ParameterMap    `json:"parameters,omitempty" jsonschema:"maxItems=1024"`
	Surfaces        []string          `json:"surfaces" jsonschema:"required,minItems=1,uniqueItems=true,enum=definitions,enum=prompt-references,enum=history-calls,enum=history-results,enum=stream-events"`
	CaseFoldInbound bool              `json:"case_fold_inbound,omitempty"`
	ToolSearch      string            `json:"tool_search,omitempty" jsonschema:"enum=regex,enum=bm25"`
}

type ToolAlias struct {
	Host     string `json:"host" jsonschema:"required,minLength=1,maxLength=128"`
	Provider string `json:"provider" jsonschema:"required,minLength=1,maxLength=128"`
}

type ToolPrefixAlias struct {
	HostPrefix        string `json:"host_prefix" jsonschema:"required,minLength=1,maxLength=128"`
	ProviderPrefix    string `json:"provider_prefix" jsonschema:"required,minLength=1,maxLength=128"`
	DeferLoading      bool   `json:"defer_loading,omitempty"`
	OmitEmptyRequired bool   `json:"omit_empty_required,omitempty"`
}

type ParameterMap struct {
	Tool     string `json:"tool" jsonschema:"required,maxLength=128"`
	Host     string `json:"host" jsonschema:"required,maxLength=128"`
	Provider string `json:"provider" jsonschema:"required,maxLength=128"`
}
