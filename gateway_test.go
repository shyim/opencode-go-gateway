package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureCatalog = `{"opencode":{"models":{
  "test":{"id":"test","type":"chat","name":"Test","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{"price":0,"cache":{"read":0}},"output":{"price":0}}},"api":{"chat-completions":{}}},
  "paid":{"id":"paid","type":"chat","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{"price":0},"output":{"price":1}}},"api":{"chat-completions":{}}},
  "tiered":{"id":"tiered","type":"chat","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{"price":0,"overrides":[{"price":1}]},"output":{"price":0}}},"api":{"chat-completions":{}}},
  "cache":{"id":"cache","type":"chat","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{"price":0,"cache":{"read":1}},"output":{"price":0}}},"api":{"chat-completions":{}}},
  "old":{"id":"old","type":"chat","status":"deprecated","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{"price":0},"output":{"price":0}}},"api":{"chat-completions":{}}},
  "missing":{"id":"missing","type":"chat","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{},"output":{"price":0}}},"api":{"chat-completions":{}}}
}},"opencode-go":{"models":{"go-free":{"id":"go-free","type":"chat","modalities":{"input":["text"],"output":["text"]},"cost":{"text":{"input":{"price":0},"output":{"price":0}}},"api":{"chat-completions":{}}}}}}`

type fixture struct {
	mu       sync.Mutex
	requests []captured
	status   int
	body     string
	catalog  string
	canceled chan struct{}
}
type captured struct {
	Path, Auth, UserAgent, Session       string
	SessionID, Affinity, Client, Project string
	Body                                 map[string]any
	Headers                              http.Header
}

func asType[T any](t *testing.T, v any) T {
	t.Helper()

	val, ok := v.(T)
	if !ok {
		t.Fatalf("expected %T, got %T (%#v)", *new(T), v, v)
	}

	return val
}

func setup(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()
	return setupWithKey(t, "test-secret")
}

func setupWithKey(t *testing.T, key string) (*fixture, *httptest.Server) {
	t.Helper()

	f := &fixture{catalog: fixtureCatalog, canceled: make(chan struct{}, 1)}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			f.mu.Lock()
			c := f.catalog
			f.mu.Unlock()

			_, _ = fmt.Fprint(w, c)

			return
		}

		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}

		var body map[string]any

		_ = json.NewDecoder(r.Body).Decode(&body)

		f.mu.Lock()
		f.requests = append(f.requests, captured{Path: r.URL.Path, Auth: r.Header.Get("Authorization"), UserAgent: r.Header.Get("User-Agent"), Session: r.Header.Get("X-Opencode-Session"), SessionID: r.Header.Get("X-Opencode-Session-Id"), Affinity: r.Header.Get("X-Session-Affinity"), Client: r.Header.Get("X-Opencode-Client"), Project: r.Header.Get("X-Opencode-Project"), Body: body})
		f.requests[len(f.requests)-1].Headers = r.Header.Clone()
		status, response := f.status, f.body
		f.mu.Unlock()

		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, response)

			return
		}

		if body["user"] == "cancel-test" {
			w.Header().Set("Content-Type", "text/event-stream")

			_, _ = fmt.Fprint(w, "data: {\"choices\":[]}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}

			<-r.Context().Done()

			f.canceled <- struct{}{}

			return
		}

		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")

			_, _ = fmt.Fprint(w, "data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"},\"finish_reason\":null}]}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}

			time.Sleep(50 * time.Millisecond)

			_, _ = fmt.Fprint(w, "data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"upstream-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"upstream-id","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(backend.Close)

	parse := func(s string) *url.URL {
		u, e := url.Parse(s)
		if e != nil {
			t.Fatal(e)
		}

		return u
	}
	g := &gateway{upstream: newUpstream(parse(backend.URL+"/catalog"), parse(backend.URL+"/console/v1"), parse(backend.URL+"/go/v1"), key), timeout: 2 * time.Second}
	srv := httptest.NewServer(g.routes())
	t.Cleanup(srv.Close)

	return f, srv
}

func TestPublicModeNeedsNoCredential(t *testing.T) {
	t.Parallel()
	f, srv := setupWithKey(t, "")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	list := asType[[]any](t, decode(t, resp)["data"])
	if len(list) != 1 || asType[map[string]any](t, list[0])["id"] != "opencode/test" {
		t.Fatalf("public models: %#v", list)
	}

	postResp := post(t, srv.URL, `{"model":"opencode/test","messages":[{"role":"user","content":"hi"}]}`, nil)
	defer func() { _ = postResp.Body.Close() }()

	data := decode(t, postResp)
	if postResp.StatusCode != http.StatusOK || data["id"] != "upstream-id" || data["object"] != "chat.completion" {
		t.Fatalf("plain response: status=%d body=%#v", postResp.StatusCode, data)
	}

	choices := asType[[]any](t, data["choices"])
	if len(choices) != 1 || asType[map[string]any](t, asType[map[string]any](t, choices[0])["message"])["content"] != "hello" {
		t.Fatalf("plain response choices: %#v", choices)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.requests) != 1 || f.requests[0].Auth != "Bearer public" {
		t.Fatalf("public request: %#v", f.requests)
	}

	body := f.requests[0].Body
	if body["stream"] != true {
		t.Fatalf("plain request was not streamed upstream: %#v", body)
	}

	messages := asType[[]any](t, body["messages"])
	if len(messages) != 2 || asType[map[string]any](t, messages[0])["role"] != "system" {
		t.Fatalf("plain request messages: %#v", messages)
	}

	if content := asType[string](t, asType[map[string]any](t, messages[0])["content"]); !strings.Contains(content, "Working directory: /workspace") {
		t.Fatalf("plain request system content: %q", content)
	}

	tools := asType[[]any](t, body["tools"])
	if len(tools) != 1 || asType[map[string]any](t, asType[map[string]any](t, tools[0])["function"])["name"] != "read" || body["tool_choice"] != "none" {
		t.Fatalf("plain request compatibility tools: %#v", body)
	}
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()

	defer func() { _ = resp.Body.Close() }()

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}

	return data
}

func post(t *testing.T, base, body string, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

func TestCatalogFiltersPaidTiersAndDeprecated(t *testing.T) {
	t.Parallel()
	_, srv := setup(t)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	list := asType[[]any](t, decode(t, resp)["data"])
	if len(list) != 2 || asType[map[string]any](t, list[0])["id"] != "opencode-go/go-free" || asType[map[string]any](t, list[1])["id"] != "opencode/test" {
		t.Fatalf("models: %#v", list)
	}

	resp = post(t, srv.URL, `{"model":"paid","messages":[{"role":"user","content":"hello"}]}`, nil)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("paid model status %d", resp.StatusCode)
	}
}

func TestForwardsStructuredChatAndToolCalls(t *testing.T) {
	t.Parallel()
	f, srv := setup(t)
	body := `{"model":"opencode/test","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}],"temperature":0.5,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`

	resp := post(t, srv.URL, body, map[string]string{"X-Opencode-Session": "ses_stable", "Authorization": "Bearer client-secret"})
	defer func() { _ = resp.Body.Close() }()

	data := decode(t, resp)
	if resp.StatusCode != http.StatusOK || data["id"] != "upstream-id" {
		t.Fatalf("status=%d body=%#v", resp.StatusCode, data)
	}

	choices := asType[[]any](t, data["choices"])

	choice := asType[map[string]any](t, choices[0])

	message := asType[map[string]any](t, choice["message"])
	if choice["finish_reason"] != "stop" || message["content"] != "hello" {
		t.Fatalf("aggregated response: %#v", data)
	}

	if usage := asType[map[string]any](t, data["usage"]); usage["total_tokens"] != float64(3) {
		t.Fatalf("aggregated usage: %#v", usage)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	got := f.requests[0]
	if got.Path != "/console/v1/chat/completions" || got.Auth != "Bearer test-secret" || got.Session != "ses_stable" || got.SessionID != got.Session || got.Affinity != got.Session || got.Client != "cli" || got.Project != "global" || got.UserAgent != "opencode/latest/2.2.5/cli" || got.Body["model"] != "test" || got.Body["temperature"] != 0.5 || got.Body["tools"] == nil {
		t.Fatalf("request %#v", got)
	}

	messages := asType[[]any](t, got.Body["messages"])
	if len(messages) != 4 || asType[map[string]any](t, messages[3])["tool_call_id"] != "call_1" {
		t.Fatalf("messages %#v", messages)
	}

	if content := asType[string](t, asType[map[string]any](t, messages[0])["content"]); !strings.HasPrefix(content, "be brief") || !strings.Contains(content, "<env>") {
		t.Fatalf("system content %q", content)
	}

	if tools := asType[[]any](t, got.Body["tools"]); len(tools) != 1 || asType[map[string]any](t, asType[map[string]any](t, tools[0])["function"])["name"] != "lookup" {
		t.Fatalf("caller tools changed: %#v", tools)
	}
}

func TestGoModelUsesGoEndpointAndFreshSession(t *testing.T) {
	t.Parallel()
	f, srv := setup(t)
	resp := post(t, srv.URL, `{"model":"opencode-go/go-free","messages":[{"role":"user","content":"hi"}]}`, nil)
	_ = resp.Body.Close()

	f.mu.Lock()
	defer f.mu.Unlock()

	got := f.requests[0]
	if got.Path != "/go/v1/chat/completions" || !strings.HasPrefix(got.Session, "ses_") || len(got.Session) != 30 {
		t.Fatalf("request %#v", got)
	}

	messages := asType[[]any](t, got.Body["messages"])
	if len(messages) != 1 || asType[map[string]any](t, messages[0])["content"] != "hi" {
		t.Fatalf("go messages changed: %#v", messages)
	}

	if _, ok := got.Body["tools"]; ok {
		t.Fatalf("go tools changed: %#v", got.Body)
	}

	if _, ok := got.Body["stream"]; ok {
		t.Fatalf("go request was streamed: %#v", got.Body)
	}
}

func TestPreservesChildSessionAndProject(t *testing.T) {
	t.Parallel()
	f, srv := setup(t)

	resp := post(t, srv.URL, `{"model":"test","messages":[{"role":"user","content":"hi"}]}`, map[string]string{
		"X-Opencode-Session-Id":        "ses_child",
		"X-Opencode-Session":           "ses_parent",
		"X-Opencode-Parent-Session-Id": "ses_parent",
		"X-Opencode-Project":           "abc123",
	})
	defer func() { _ = resp.Body.Close() }()

	if resp.Header.Get("X-Opencode-Session-Id") != "ses_child" || resp.Header.Get("X-Opencode-Session") != "ses_parent" {
		t.Fatalf("response identity: %v", resp.Header)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	got := f.requests[0]
	if got.SessionID != "ses_child" || got.Session != "ses_parent" || got.Affinity != "ses_parent" || got.Project != "abc123" || got.Headers.Get("X-Session-Id") != "ses_parent" || got.Headers.Get("X-Parent-Session-Id") != "ses_parent" || got.Headers.Get("X-Opencode-Parent-Session-Id") != "ses_parent" || got.Headers.Get("Accept") != "*/*" {
		t.Fatalf("request identity: %#v", got)
	}
}

func TestIdentityAliasesAndValidation(t *testing.T) {
	t.Parallel()

	for _, header := range []string{"X-Session-Affinity", "X-Session-Id"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
		r.Header.Set("X-Opencode-Session-Id", "ses_fork")
		r.Header.Set(header, "ses_original")

		got, err := identity(r)
		if err != nil || got.session != "ses_fork" || got.affinity != "ses_original" {
			t.Fatalf("%s: %#v, %v", header, got, err)
		}
	}

	for _, header := range []string{"X-Opencode-Session-Id", "X-Opencode-Session", "X-Session-Affinity", "X-Opencode-Parent-Session-Id", "X-Opencode-Project"} {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
		r.Header.Set(header, "bad/value")

		if _, err := identity(r); err == nil {
			t.Fatalf("accepted invalid %s", header)
		}
	}
}

func TestStreamingIsForwardedIncrementally(t *testing.T) {
	t.Parallel()
	_, srv := setup(t)

	resp := post(t, srv.URL, `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	defer func() { _ = resp.Body.Close() }()

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}

	scan := bufio.NewScanner(resp.Body)
	if !scan.Scan() || !strings.Contains(scan.Text(), "hel") {
		t.Fatalf("first line %q", scan.Text())
	}

	found := false

	for scan.Scan() {
		if strings.Contains(scan.Text(), "[DONE]") {
			found = true
		}
	}

	if !found || scan.Err() != nil {
		t.Fatalf("stream ended before DONE: %v", scan.Err())
	}
}

func TestClientCancellationClosesUpstream(t *testing.T) {
	t.Parallel()
	f, srv := setup(t)
	resp := post(t, srv.URL, `{"model":"test","stream":true,"user":"cancel-test","messages":[{"role":"user","content":"hi"}]}`, nil)

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	select {
	case <-f.canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request was not canceled")
	}
}

func TestForwardsUpstreamErrors(t *testing.T) {
	t.Parallel()
	f, srv := setup(t)
	f.mu.Lock()
	f.status = 429
	f.body = `{"error":{"message":"free limit"}}`
	f.mu.Unlock()

	resp := post(t, srv.URL, `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	defer func() { _ = resp.Body.Close() }()

	data := decode(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(fmt.Sprint(data), "free limit") {
		t.Fatalf("status=%d body=%#v", resp.StatusCode, data)
	}
}

func TestRejectsInvalidInputAndProtectsAuth(t *testing.T) {
	t.Parallel()

	_, srv := setup(t)
	for _, body := range []string{`{"model":"test","messages":[]}`, `{"model":"test","messages":[{}],"service_tier":"priority"}`, `{"model":"test","messages":[{}]} {}`, `null`} {
		resp := post(t, srv.URL, body, nil)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %s status %d", body, resp.StatusCode)
		}
	}

	resp := post(t, srv.URL, `{"model":"test","messages":[{}]}`, map[string]string{"X-Opencode-Session": "bad/session"})
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("session status %d", resp.StatusCode)
	}

	u, _ := url.Parse("http://127.0.0.1")
	g := &gateway{apiKey: "secret", upstream: newUpstream(u, u, u, "public"), timeout: time.Second}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/v1/models", nil)
	out := httptest.NewRecorder()
	g.routes().ServeHTTP(out, req)

	if out.Code != 401 {
		t.Fatalf("auth status %d", out.Code)
	}
}
