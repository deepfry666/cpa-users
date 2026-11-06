package main

// Wire types for the CLIProxyAPI plugin RPC contract.
//
// The plugin deliberately does not import the CLIProxyAPI module: the host and
// plugin exchange JSON across a C ABI, so mirroring the handful of fields we
// use keeps the plugin small and tolerant of host upgrades. Host-side structs
// without json tags are encoded with their Go field names, which is why most
// tags below are PascalCase.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

const PluginID = "cpa-users"

// PluginVersion is stamped by the release build (-X main.PluginVersion=...) so the
// registered version always matches the release tag the store installs.
var PluginVersion = "0.1.0"

const (
	abiVersion uint32 = 1
	// Schema 7 is the first version that lets frontend auth and request
	// interceptors opt out of receiving request bodies.
	schemaVersion uint32 = 7
)

const (
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginShutdown    = "plugin.shutdown"
	methodPluginQuiesce     = "plugin.quiesce"

	methodFrontendAuthIdentifier   = "frontend_auth.identifier"
	methodFrontendAuthAuthenticate = "frontend_auth.authenticate"

	methodRequestInterceptBefore = "request.intercept_before"
	methodRequestInterceptAfter  = "request.intercept_after"
	methodRequestComplete        = "request.complete"
	methodResponseInterceptAfter = "response.intercept_after"

	methodUsageHandle = "usage.handle"

	methodManagementRegister = "management.register"
	methodManagementHandle   = "management.handle"

	methodHostAuthModels = "host.auth.models"
)

// Metadata keys the host places on request interception metadata.
const (
	metaCallerScope = "caller_scope"
	metaGenerate    = "generate"
	metaSource      = "source"
	// sourceHostModelCallback marks nested model executions started by a
	// plugin; they are not client traffic and bypass admission.
	sourceHostModelCallback = "plugin_host_model_callback"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository,omitempty"`
	Logo             string        `json:"Logo,omitempty"`
	ConfigFields     []configField `json:"ConfigFields,omitempty"`
}

type configField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	Default     any      `json:"Default,omitempty"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description,omitempty"`
}

type capabilities struct {
	FrontendAuthProvider             bool `json:"frontend_auth_provider"`
	FrontendAuthProviderOmitBody     bool `json:"frontend_auth_provider_omit_body,omitempty"`
	RequestInterceptor               bool `json:"request_interceptor"`
	RequestInterceptorOmitBodyBefore bool `json:"request_interceptor_omit_body_before,omitempty"`
	RequestInterceptorOmitBodyAfter  bool `json:"request_interceptor_omit_body_after,omitempty"`
	RequestLifecyclePlugin           bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor              bool `json:"response_interceptor"`
	UsagePlugin                      bool `json:"usage_plugin"`
	ManagementAPI                    bool `json:"management_api"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type frontendAuthRequest struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
}

type frontendAuthResponse struct {
	Authenticated  bool              `json:"Authenticated"`
	Principal      string            `json:"Principal,omitempty"`
	Metadata       map[string]string `json:"Metadata,omitempty"`
	DenyStatusCode int               `json:"DenyStatusCode,omitempty"`
	DenyMessage    string            `json:"DenyMessage,omitempty"`
}

type requestInterceptRequest struct {
	RequestID      string         `json:"RequestID"`
	SourceFormat   string         `json:"SourceFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	CallerScope    string         `json:"CallerScope"`
	Metadata       map[string]any `json:"Metadata"`
}

type requestInterceptResponse struct {
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

type requestCompletion struct {
	RequestID  string `json:"RequestID"`
	Outcome    string `json:"Outcome"`
	StatusCode int    `json:"StatusCode"`
}

type responseInterceptRequest struct {
	Model          string      `json:"Model"`
	RequestedModel string      `json:"RequestedModel"`
	SourceFormat   string      `json:"SourceFormat"`
	RequestHeaders http.Header `json:"RequestHeaders"`
	Body           []byte      `json:"Body"`
	StatusCode     int         `json:"StatusCode"`
}

type responseInterceptResponse struct {
	Body []byte `json:"Body,omitempty"`
}

type usageRecord struct {
	RequestID       string        `json:"RequestID"`
	Provider        string        `json:"Provider"`
	Model           string        `json:"Model"`
	Alias           string        `json:"Alias"`
	APIKey          string        `json:"APIKey"`
	AccessProvider  string        `json:"AccessProvider"`
	Generate        bool          `json:"Generate"`
	Stream          bool          `json:"Stream"`
	RequestedAt     time.Time     `json:"RequestedAt"`
	Latency         time.Duration `json:"Latency"`
	TTFT            time.Duration `json:"TTFT"`
	Failed          bool          `json:"Failed"`
	Failure         usageFailure  `json:"Failure"`
	Detail          usageDetail   `json:"Detail"`
	ReasoningEffort string        `json:"ReasoningEffort"`
}

type usageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body"`
}

type usageDetail struct {
	InputTokens     int64 `json:"InputTokens"`
	OutputTokens    int64 `json:"OutputTokens"`
	ReasoningTokens int64 `json:"ReasoningTokens"`
	CachedTokens    int64 `json:"CachedTokens"`
	TotalTokens     int64 `json:"TotalTokens"`
}

type managementRegistrationRequest struct {
	BasePath         string `json:"BasePath"`
	ResourceBasePath string `json:"ResourceBasePath"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRequest struct {
	Method         string      `json:"Method"`
	Path           string      `json:"Path"`
	Headers        http.Header `json:"Headers"`
	Query          url.Values  `json:"Query"`
	Body           []byte      `json:"Body"`
	HostCallbackID string      `json:"host_callback_id,omitempty"`
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

type hostAuthModelsResponse struct {
	Credentials []struct {
		Provider string   `json:"provider"`
		Models   []string `json:"models"`
	} `json:"credentials"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
