package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"reflect"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// charpy stamps W3C trace context on every request and notification its
// peers originate, on both transports, in _meta and -- over HTTP -- in the
// traceparent header too (ADR-005, ADR-014, SEP-414). A gateway that forwards
// a message carries the trace across, and that is the authoritative join
// between its faces.
//
// The ids come from Options.Trace, which the driver backs with a seeded
// stream, so a citation reproduces charpy's requests byte for byte.

// stamp is the sending middleware that puts a fresh traceparent in the
// _meta of each message the peer originates. Notifications too (ADR-014):
// unstamped, a notification charpy sent is a frame of charpy's with no join
// key, and its other half -- if a gateway forwards it -- has nothing to join
// to but a content match.
func stamp(next func() string) mcp.Middleware {
	return func(h mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if p := params(method, req); p != nil {
				meta := maps.Clone(p.GetMeta())
				if meta == nil {
					meta = map[string]any{}
				}
				meta["traceparent"] = next()
				p.SetMeta(meta)
			}
			return h(ctx, method, req)
		}
	}
}

// params is a message's params, allocated if the caller left them nil -- a
// ping, a listing, notifications/initialized -- so there is somewhere to put
// _meta. The SDK then holds them as the Params interface with nothing in it,
// and Params cannot be implemented outside the SDK, so the empty value comes
// from the method: the messages charpy's peers send without params. Any
// other message left without params goes out unstamped, and is recorded as
// joining nothing rather than as traced.
func params(method string, req mcp.Request) mcp.Params {
	if p := req.GetParams(); p != nil && !reflect.ValueOf(p).IsNil() {
		return p
	}
	empty, ok := emptyParams[method]
	if !ok {
		return nil
	}
	v := reflect.ValueOf(req)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return nil
	}
	f := v.Elem().FieldByName("Params")
	p := empty()
	if !f.IsValid() || !f.CanSet() || !reflect.TypeOf(p).AssignableTo(f.Type()) {
		return nil
	}
	f.Set(reflect.ValueOf(p))
	return p
}

var emptyParams = map[string]func() mcp.Params{
	"ping":                     func() mcp.Params { return &mcp.PingParams{} },
	"tools/list":               func() mcp.Params { return &mcp.ListToolsParams{} },
	"prompts/list":             func() mcp.Params { return &mcp.ListPromptsParams{} },
	"resources/list":           func() mcp.Params { return &mcp.ListResourcesParams{} },
	"resources/templates/list": func() mcp.Params { return &mcp.ListResourceTemplatesParams{} },
	"roots/list":               func() mcp.Params { return &mcp.ListRootsParams{} },

	"notifications/initialized":            func() mcp.Params { return &mcp.InitializedParams{} },
	"notifications/roots/list_changed":     func() mcp.Params { return &mcp.RootsListChangedParams{} },
	"notifications/tools/list_changed":     func() mcp.Params { return &mcp.ToolListChangedParams{} },
	"notifications/prompts/list_changed":   func() mcp.Params { return &mcp.PromptListChangedParams{} },
	"notifications/resources/list_changed": func() mcp.Params { return &mcp.ResourceListChangedParams{} },
}

// traceHeader copies a request body's _meta.traceparent into the HTTP
// traceparent header, which is where an HTTP-level tracer looks for it.
type traceHeader struct{ base http.RoundTripper }

func (t traceHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

	var m struct {
		Params struct {
			Meta struct {
				Traceparent string `json:"traceparent"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &m) == nil && m.Params.Meta.Traceparent != "" {
		req.Header.Set("Traceparent", m.Params.Meta.Traceparent)
	}
	return t.base.RoundTrip(req)
}
