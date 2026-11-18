package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decodeFrames(t *testing.T, output string) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("stdout line is not valid JSON: %q: %v", line, err)
		}
		if frame["jsonrpc"] != "2.0" {
			t.Fatalf("stdout frame missing jsonrpc 2.0: %v", frame)
		}
		frames = append(frames, frame)
	}
	return frames
}

func frameByID(frames []map[string]any, id float64) map[string]any {
	for _, frame := range frames {
		if value, ok := frame["id"].(float64); ok && value == id {
			return frame
		}
	}
	return nil
}

func TestServerStdoutOnlyProtocolFrames(t *testing.T) {
	logBuf := &bytes.Buffer{}
	server := NewServer([]Tool{{
		Name:        "echo",
		Description: "echo tool",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	}}, logBuf)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"does/not/exist"}`,
		`{this is not json`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"missing","arguments":{}}}`,
	}, "\n")
	var out bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	if logBuf.Len() != 0 {
		t.Fatalf("server wrote %q to log writer during normal operation", logBuf.String())
	}
	frames := decodeFrames(t, out.String())
	if len(frames) != 7 {
		t.Fatalf("response frames = %d, want 7 (notification must not answer): %v", len(frames), frames)
	}

	init := frameByID(frames, 1)
	if init == nil {
		t.Fatal("missing initialize response")
	}
	initResult, _ := init["result"].(map[string]any)
	if initResult["protocolVersion"] != "2025-03-26" {
		t.Fatalf("protocolVersion = %v, want 2025-03-26", initResult["protocolVersion"])
	}
	serverInfo, _ := initResult["serverInfo"].(map[string]any)
	if serverInfo["name"] != ServerName {
		t.Fatalf("serverInfo = %v", serverInfo)
	}

	list := frameByID(frames, 2)
	listResult, _ := list["result"].(map[string]any)
	tools, _ := listResult["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want one", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "echo" || tool["inputSchema"] == nil {
		t.Fatalf("tool descriptor = %v", tool)
	}

	call := frameByID(frames, 4)
	callResult, _ := call["result"].(map[string]any)
	if callResult["isError"] != false {
		t.Fatalf("echo tool isError = %v", callResult["isError"])
	}
	structured, _ := callResult["structuredContent"].(map[string]any)
	if structured["ok"] != true {
		t.Fatalf("structuredContent = %v", structured)
	}

	unknownMethod := frameByID(frames, 5)
	rpcErr, _ := unknownMethod["error"].(map[string]any)
	if rpcErr == nil || rpcErr["code"] != float64(-32601) {
		t.Fatalf("unknown method error = %v", unknownMethod)
	}

	var parseError map[string]any
	for _, frame := range frames {
		if _, hasID := frame["id"]; hasID {
			continue
		}
		if errObj, ok := frame["error"].(map[string]any); ok && errObj["code"] == float64(-32700) {
			parseError = frame
			break
		}
	}
	if parseError == nil {
		t.Fatalf("missing -32700 parse error frame: %v", frames)
	}

	unknownTool := frameByID(frames, 6)
	rpcErr, _ = unknownTool["error"].(map[string]any)
	if rpcErr == nil || rpcErr["code"] != float64(-32602) {
		t.Fatalf("unknown tool error = %v", unknownTool)
	}
}

func TestServerToolErrorEnvelopeStaysOnProtocol(t *testing.T) {
	logBuf := &bytes.Buffer{}
	server := NewServer([]Tool{{
		Name: "fail",
		Handler: func(context.Context, json.RawMessage) (any, error) {
			return nil, errors.New("boom")
		},
	}}, logBuf)
	var out bytes.Buffer
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail","arguments":{}}}`
	if err := server.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	frames := decodeFrames(t, out.String())
	if len(frames) != 1 {
		t.Fatalf("frames = %v, want 1", frames)
	}
	result, _ := frames[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", content)
	}
	first, _ := content[0].(map[string]any)
	if !strings.Contains(first["text"].(string), "boom") {
		t.Fatalf("content text = %v", first["text"])
	}
	if logBuf.Len() != 0 {
		t.Fatalf("tool error leaked to log writer: %q", logBuf.String())
	}
}

func TestServerNotificationGetsNoResponse(t *testing.T) {
	server := NewServer(nil, nil)
	var out bytes.Buffer
	input := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	if err := server.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("notification produced output: %q", out.String())
	}
}

func TestServerInitializeDefaultProtocol(t *testing.T) {
	server := NewServer(nil, nil)
	var out bytes.Buffer
	input := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	if err := server.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	frames := decodeFrames(t, out.String())
	result, _ := frames[0]["result"].(map[string]any)
	if result["protocolVersion"] != DefaultProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", result["protocolVersion"], DefaultProtocolVersion)
	}
}
