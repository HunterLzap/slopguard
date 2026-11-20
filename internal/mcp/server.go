// Package mcp implements a small stdio JSON-RPC 2.0 server for SlopGuard.
//
// The transport is newline-delimited JSON: every request and response is a
// single JSON object on its own line. The server never writes logs to stdout;
// callers pass a separate log writer (normally stderr).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
)

type sessionRootContextKey struct{}

// WithSessionRoot attaches an MCP client-advertised workspace root to ctx.
func WithSessionRoot(ctx context.Context, root string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, sessionRootContextKey{}, strings.TrimSpace(root))
}

// SessionRootFromContext returns any workspace root parsed from MCP initialize.
func SessionRootFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(sessionRootContextKey{}).(string)
	return strings.TrimSpace(v)
}

const (
	// DefaultProtocolVersion is returned when the client does not request one.
	DefaultProtocolVersion = "2024-11-05"
	// ServerName is the MCP serverInfo name.
	ServerName = "slopguard"
	// ServerVersion is the MCP serverInfo version.
	ServerVersion = "0.1.0"
)

// Tool is one MCP tool exposed by the server.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, args json.RawMessage) (any, error)
}

// Server is a stdio MCP server with a fixed tool set.
type Server struct {
	Tools       []Tool
	Log         io.Writer
	Protocol    string
	ServerInfo  string
	sessionRoot string
}

// NewServer creates a server. log may be nil; log messages are discarded then.
func NewServer(tools []Tool, log io.Writer) *Server {
	return &Server{
		Tools:      tools,
		Log:        log,
		Protocol:   DefaultProtocolVersion,
		ServerInfo: ServerName,
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Serve reads newline-delimited JSON-RPC requests from in and writes responses
// to out until EOF or context cancellation.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	writer := bufio.NewWriter(out)
	defer writer.Flush()

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request rpcRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			// A malformed frame has no reliable id; report a protocol error.
			if err := writeResponse(writer, rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
			}); err != nil {
				return err
			}
			continue
		}
		if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
			if len(request.ID) == 0 {
				continue
			}
			if err := writeResponse(writer, rpcResponse{
				JSONRPC: "2.0",
				ID:      request.ID,
				Error:   &rpcError{Code: -32600, Message: "invalid request"},
			}); err != nil {
				return err
			}
			continue
		}

		result, rpcErr := s.dispatch(ctx, request)
		if len(request.ID) == 0 {
			// Notifications never get a response.
			continue
		}
		if rpcErr != nil {
			if err := writeResponse(writer, rpcResponse{JSONRPC: "2.0", ID: request.ID, Error: rpcErr}); err != nil {
				return err
			}
			continue
		}
		if err := writeResponse(writer, rpcResponse{JSONRPC: "2.0", ID: request.ID, Result: result}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("mcp: read stdin: %w", err)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, request rpcRequest) (any, *rpcError) {
	switch request.Method {
	case "initialize":
		protocol := s.Protocol
		if protocol == "" {
			protocol = DefaultProtocolVersion
		}
		var params struct {
			ProtocolVersion  string `json:"protocolVersion"`
			RootURI          string `json:"rootUri"`
			RootPath         string `json:"rootPath"`
			WorkspaceFolders []struct {
				URI string `json:"uri"`
			} `json:"workspaceFolders"`
		}
		if len(request.Params) > 0 {
			_ = json.Unmarshal(request.Params, &params)
			if strings.TrimSpace(params.ProtocolVersion) != "" {
				protocol = params.ProtocolVersion
			}
			for _, candidate := range append(workspaceFolderURIs(params.WorkspaceFolders), params.RootURI, params.RootPath) {
				if parsed := parseWorkspaceRootCandidate(candidate); parsed != "" {
					s.sessionRoot = parsed
					break
				}
			}
		}
		return map[string]any{
			"protocolVersion": protocol,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    ServerName,
				"version": ServerVersion,
			},
			"instructions": "SlopGuard deterministic code-quality guardrails. Use sg_verify after edits; record repeated AI mistakes with sg_record_mistake; propose rules only after sg_dry_run_rule and explicit user confirmation before sg_apply_rule.",
		}, nil
	case "notifications/initialized", "initialized":
		return map[string]any{}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.toolDescriptors()}, nil
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tools/call params: " + err.Error()}
		}
		tool := s.findTool(params.Name)
		if tool == nil {
			return nil, &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}
		}
		args := params.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		callCtx := ctx
		if s.sessionRoot != "" && SessionRootFromContext(callCtx) == "" {
			callCtx = WithSessionRoot(callCtx, s.sessionRoot)
		}
		value, err := tool.Handler(callCtx, args)
		if err != nil {
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": err.Error()}},
				"isError": true,
			}, nil
		}
		text, err := json.Marshal(value)
		if err != nil {
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": "encode result: " + err.Error()}},
				"isError": true,
			}, nil
		}
		return map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(text)}},
			"structuredContent": value,
			"isError":           false,
		}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + request.Method}
	}
}

func (s *Server) findTool(name string) *Tool {
	for i := range s.Tools {
		if s.Tools[i].Name == name {
			return &s.Tools[i]
		}
	}
	return nil
}

func (s *Server) toolDescriptors() []map[string]any {
	out := make([]map[string]any, 0, len(s.Tools))
	for _, tool := range s.Tools {
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": schema,
		})
	}
	return out
}

func workspaceFolderURIs(folders []struct {
	URI string `json:"uri"`
}) []string {
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.URI)
	}
	return out
}

func parseWorkspaceRootCandidate(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "file://") {
		u, err := url.Parse(trimmed)
		if err != nil || u.Path == "" {
			return ""
		}
		p := u.Path
		if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:]
		}
		return filepath.Clean(filepath.FromSlash(p))
	}
	return filepath.Clean(trimmed)
}

func writeResponse(w *bufio.Writer, response rpcResponse) error {
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	return w.Flush()
}
