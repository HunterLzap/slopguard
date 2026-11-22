package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/HunterLzap/slopguard/internal/contract"
)

const protocolVersion = "2025-11-25"

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpTool(name, description string, extra map[string]any, required ...string) toolSpec {
	properties := map[string]any{"project_path": map[string]any{"type": "string", "description": "Absolute path to the project."}}
	for name, spec := range extra {
		properties[name] = spec
	}
	return toolSpec{Name: name, Description: description, InputSchema: map[string]any{
		"type": "object", "properties": properties, "required": append([]string{"project_path"}, required...), "additionalProperties": false,
	}}
}

var mcpTools = []toolSpec{
	mcpTool("sg_verify", "Run the selected checks and record diagnostics.", map[string]any{
		"scope": map[string]any{"type": "string", "enum": []string{"changed", "all"}, "default": "changed"},
	}),
	mcpTool("sg_status", "Read project status and enabled rule and pending proposal counts.", nil),
	mcpTool("sg_list_proposals", "List proposals ready for user review.", nil),
	mcpTool("sg_apply_proposal", "Apply a pending proposal after explicit user approval.", map[string]any{
		"proposal_id": map[string]string{"type": "string"},
		"confirm":     map[string]any{"type": "boolean", "const": true},
	}, "proposal_id", "confirm"),
	mcpTool("sg_dismiss_proposal", "Dismiss or snooze a pending proposal.", map[string]any{
		"proposal_id": map[string]string{"type": "string"},
		"mode":        map[string]any{"type": "string", "enum": []string{"dismiss", "snooze"}},
	}, "proposal_id", "mode"),
}

type rpcRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type toolReport struct {
	Content           []map[string]string `json:"content"`
	StructuredContent any                 `json:"structuredContent,omitempty"`
	IsError           bool                `json:"isError,omitempty"`
}

func (s *Service) ServeMCP(ctx context.Context, input io.Reader, output, logs io.Writer) error {
	worker, stop := context.WithCancel(ctx)
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.reviewer.RunQueue(worker, wake, func(err error) { fmt.Fprintln(logs, "review queue:", err) })
	}()
	defer func() { stop(); <-done }()
	trigger := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	trigger()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	encoder := json.NewEncoder(output)
	var initialized bool
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		response := rpcResponse{Version: "2.0", ID: json.RawMessage("null")}
		data := bytes.TrimSpace(scanner.Bytes())
		var request rpcRequest
		switch {
		case !json.Valid(data):
			response.Error = &rpcError{-32700, "Parse error"}
		case len(data) == 0 || data[0] != '{' || json.Unmarshal(data, &request) != nil || request.Version != "2.0" ||
			request.Method == "" || !rpcIDValid(request.ID):
			response.Error = &rpcError{-32600, "Invalid Request"}
		case len(request.ID) == 0:
			continue
		default:
			response.ID = request.ID
			switch request.Method {
			case "initialize":
				var p struct {
					ProtocolVersion string `json:"protocolVersion"`
				}
				if initialized || json.Unmarshal(request.Params, &p) != nil || p.ProtocolVersion == "" {
					response.Error = &rpcError{-32602, "Invalid initialization parameters"}
					break
				}
				if p.ProtocolVersion != "2024-11-05" && p.ProtocolVersion != "2025-03-26" &&
					p.ProtocolVersion != "2025-06-18" && p.ProtocolVersion != protocolVersion {
					p.ProtocolVersion = protocolVersion
				}
				initialized = true
				response.Result = map[string]any{
					"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]bool{"listChanged": false}},
					"serverInfo": map[string]string{"name": "slopguard", "version": "2.0.0"},
				}
			case "ping":
				response.Result = map[string]any{}
			case "tools/list", "tools/call":
				if !initialized {
					response.Error = &rpcError{-32000, "Initialize before calling tools"}
				} else if request.Method == "tools/list" {
					response.Result = map[string]any{"tools": mcpTools}
				} else {
					response.Result, response.Error = s.callTool(ctx, request.Params, trigger)
				}
			default:
				response.Error = &rpcError{-32601, "Method not found"}
			}
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func rpcIDValid(id json.RawMessage) bool {
	if len(id) == 0 || id[0] == '"' {
		return true
	}
	return (id[0] == '-' || id[0] >= '0' && id[0] <= '9') && !bytes.ContainsAny(id, "eE.")
}

func (s *Service) callTool(ctx context.Context, raw json.RawMessage, wake func()) (any, *rpcError) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	var args struct {
		ProjectPath string               `json:"project_path"`
		Scope       contract.VerifyScope `json:"scope"`
		ID          string               `json:"proposal_id"`
		Confirm     bool                 `json:"confirm"`
		Mode        string               `json:"mode"`
	}
	if json.Unmarshal(raw, &call) != nil || call.Name == "" || json.Unmarshal(call.Arguments, &args) != nil ||
		strings.TrimSpace(args.ProjectPath) == "" {
		return nil, &rpcError{-32602, "Tool name and project_path are required"}
	}
	var output any
	var err error
	switch call.Name {
	case "sg_verify":
		if args.Scope == "" {
			args.Scope = contract.ScopeChanged
		}
		output, err = s.Verify(ctx, args.ProjectPath, args.Scope, false)
		if err == nil {
			wake()
		}
	case "sg_status":
		output, err = s.Status(ctx, args.ProjectPath)
	case "sg_list_proposals":
		var proposals []contract.RuleProposal
		proposals, err = s.Proposals(ctx, args.ProjectPath)
		output = map[string]any{"proposals": proposals}
	case "sg_apply_proposal":
		if !args.Confirm {
			err = fmt.Errorf("confirm must be true after the user approves the proposal")
		} else {
			output, err = s.Approve(ctx, args.ProjectPath, args.ID)
		}
	case "sg_dismiss_proposal":
		output, err = s.Dismiss(ctx, args.ProjectPath, args.ID, args.Mode)
	default:
		return nil, &rpcError{-32602, "Unknown tool"}
	}
	if err != nil {
		return toolReport{Content: []map[string]string{{"type": "text", "text": err.Error()}}, IsError: true}, nil
	}
	jsonText, err := json.Marshal(output)
	if err != nil {
		return nil, &rpcError{-32603, "Cannot encode tool result"}
	}
	return toolReport{Content: []map[string]string{{"type": "text", "text": string(jsonText)}}, StructuredContent: output}, nil
}
