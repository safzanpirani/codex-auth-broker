package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCursorShimRunConnectJSON(t *testing.T) {
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("missing bearer header: %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"id\":\"resp_test\"}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n")
		fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"delta\":\" world\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	shim := newCursorShim(&responsesProxy{cfg: config{models: []string{"local"}, upstreamURL: upstream.URL}, auth: testAuthManager(t), client: upstream.Client()})
	server := httptest.NewServer(http.HandlerFunc(shim.handle))
	defer server.Close()

	payload := mustJSON(map[string]any{"stream_unified_chat_request": map[string]any{"conversation": []any{map[string]any{"text": "say hello"}}, "model_details": map[string]any{"model_name": "local"}}})
	req, err := http.NewRequest(http.MethodPost, server.URL+"/agent.v1.AgentService/Run", strings.NewReader(string(encodeConnectFrame(payload, 0))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/connect+json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var text string
	for _, frame := range decodeConnectFrames(data) {
		text += string(frame.data) + "\n"
	}
	if !strings.Contains(text, "stream_start") || !strings.Contains(text, "hello world") || !strings.Contains(text, "conversation_summary") {
		t.Fatalf("unexpected cursor frames: %s", text)
	}
	if upstreamBody["model"] != "local" || upstreamBody["stream"] != true {
		t.Fatalf("unexpected upstream body: %#v", upstreamBody)
	}
}

func TestCursorPromptPrefersStructuredUserMessage(t *testing.T) {
	userMessage := pbConcat(pbString(1, "this is so peak"), pbString(8, "rich text fallback"))
	userAction := pbMessage(1, userMessage)
	action := pbMessage(1, userAction)
	runRequest := pbConcat(pbMessage(2, action), pbString(8, "What CLI config setting would you like to change?"))
	clientMessage := pbMessage(1, runRequest)
	if got := cursorPrompt(connectProto, clientMessage); got != "this is so peak" {
		t.Fatalf("unexpected prompt %q", got)
	}

	directAction := pbMessage(4, action)
	if got := cursorPrompt(connectProto, directAction); got != "this is so peak" {
		t.Fatalf("unexpected direct action prompt %q", got)
	}
}

func TestCursorShimServerConfigDisablesHTTP2(t *testing.T) {
	shim := newCursorShim(&responsesProxy{cfg: config{cursorListen: "127.0.0.1:8318", models: []string{"local"}}})
	resp, ok := shim.unaryStub("/aiserver.v1.ServerConfigService/GetServerConfig", connectProto, nil)
	if !ok {
		t.Fatal("missing server config stub")
	}
	if value, ok := pbVarintField(resp, 7); !ok || value != 1 {
		t.Fatalf("expected http2_config force-disabled field 7=1, got %d ok=%v", value, ok)
	}
	if cfg := firstPBBytes(resp, 27); pbStringField(cfg, 1) != "http://127.0.0.1:8318" || pbStringField(cfg, 2) != "http://127.0.0.1:8318" {
		t.Fatalf("unexpected agent_url_config: %x", cfg)
	}
}

func TestCursorShimListDirExecProtoRoundTrip(t *testing.T) {
	toolCall := &cursorToolCall{CallID: "call_123", Kind: "list_dir", Path: "/tmp", ExecID: 1}
	frame := cursorExecFrame(connectProto, toolCall)
	exec := firstPBBytes(frame, 2)
	if len(exec) == 0 {
		t.Fatalf("expected AgentServerMessage exec payload: %x", frame)
	}
	if got := pbStringField(exec, 15); got != "call_123" {
		t.Fatalf("unexpected exec_id %q", got)
	}
	args := firstPBBytes(exec, 8)
	if got := pbStringField(args, 1); got != "/tmp" {
		t.Fatalf("unexpected ls path %q", got)
	}

	root := pbConcat(
		pbString(1, "/tmp"),
		pbMessage(2, pbConcat(pbString(1, "subdir"), pbMessage(3, pbString(1, "nested.txt")))),
		pbMessage(3, pbString(1, "codex.js")),
	)
	lsResult := pbMessage(1, pbMessage(1, root))
	result := cursorToolResult(connectProto, pbMessage(2, pbMessage(8, lsResult)), toolCall)
	if !strings.Contains(result.Output, "[dir] subdir") || !strings.Contains(result.Output, "nested.txt") || !strings.Contains(result.Output, "codex.js") {
		t.Fatalf("unexpected parsed ls result: %q", result.Output)
	}
}

func TestCursorShimReadWriteShellProtoFrames(t *testing.T) {
	readCall := &cursorToolCall{CallID: "call_read", Kind: "read", Path: "README.md", ExecID: 2}
	readExec := firstPBBytes(cursorExecFrame(connectProto, readCall), 2)
	if args := firstPBBytes(readExec, 7); pbStringField(args, 1) != "README.md" || pbStringField(args, 2) != "call_read" {
		t.Fatalf("bad read args: %x", args)
	}
	readResult := cursorToolResult(connectProto, pbMessage(2, pbConcat(pbString(15, "call_read"), pbMessage(7, pbMessage(1, pbString(2, "hello file"))))), readCall)
	if readResult.Output != "hello file" {
		t.Fatalf("bad read result: %q", readResult.Output)
	}

	writeCall := &cursorToolCall{CallID: "call_write", Kind: "write", Path: "/tmp/test.txt", Args: `{"content":"hello"}`, ExecID: 3}
	writeExec := firstPBBytes(cursorExecFrame(connectProto, writeCall), 2)
	if args := firstPBBytes(writeExec, 3); pbStringField(args, 1) != "/tmp/test.txt" || pbStringField(args, 2) != "hello" {
		t.Fatalf("bad write args: %x", args)
	}

	shellCall := &cursorToolCall{CallID: "call_shell", Kind: "shell", Args: `{"command":"pwd"}`, ExecID: 4}
	shellExec := firstPBBytes(cursorExecFrame(connectProto, shellCall), 2)
	if args := firstPBBytes(shellExec, 2); pbStringField(args, 1) != "pwd" {
		t.Fatalf("bad shell args: %x", args)
	}
}

func TestCursorBidiAppendProtoHexData(t *testing.T) {
	payload := []byte("hello")
	body := pbConcat(pbString(1, hex.EncodeToString(payload)), pbMessage(2, pbString(1, "req_123")))
	requestID, data := cursorBidiAppend(connectProto, body)
	if requestID != "req_123" || string(data) != "hello" {
		t.Fatalf("unexpected BidiAppend parse id=%q data=%q", requestID, string(data))
	}
}

func testAuthManager(t *testing.T) *authManager {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	token := testJWT(t)
	doc := map[string]any{"tokens": map[string]any{"access_token": token, "refresh_token": "refresh", "account_id": "acct_test"}}
	data, _ := json.Marshal(doc)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return &authManager{authFile: path, refreshSkew: time.Minute, client: http.DefaultClient}
}

func testJWT(t *testing.T) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		"iat":                         time.Now().Add(-time.Minute).Unix(),
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
