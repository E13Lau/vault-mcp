package deepsearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenRouterRequestAndScores(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"vendor/judge-1","choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":0.9},{\"id\":2,\"score\":0.25}]}"}}],"usage":{"prompt_tokens":1200,"completion_tokens":40,"cost":0.00042}}`)
	}))
	defer srv.Close()
	sc := NewOpenRouter(srv.URL+"/chat/completions", "vendor/judge", "test-key")
	scores, usage, err := sc.Score(context.Background(), "崩溃率是多少？", []Passage{
		{ID: 1, Title: "iOS", Heading: "项目经验", Text: "Connect 崩溃率 0.6%。"},
		{ID: 2, Title: "其他", Text: "无关内容。"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer test-key" {
		t.Fatalf("authorization = %q", auth)
	}
	if got["model"] != "vendor/judge" {
		t.Fatalf("model = %v", got["model"])
	}
	if rf, ok := got["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Fatalf("response_format = %v", got["response_format"])
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	system, _ := messages[0].(map[string]any)
	if !strings.Contains(system["content"].(string), "Score each candidate") {
		t.Fatal("system prompt is not the judge criterion")
	}
	user, _ := messages[1].(map[string]any)
	for _, want := range []string{"崩溃率是多少？", "[id 1] title: iOS", "heading: 项目经验", "[id 2] title: 其他"} {
		if !strings.Contains(user["content"].(string), want) {
			t.Fatalf("user message missing %q:\n%s", want, user["content"])
		}
	}
	if len(scores) != 2 || scores[0].Value != 0.9 || scores[1].Value != 0.25 {
		t.Fatalf("scores = %+v", scores)
	}
	if usage.Requests != 1 || usage.InputTokens != 1200 || usage.OutputTokens != 40 || usage.CostUSD != 0.00042 || usage.Model != "vendor/judge-1" {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestScoreFallbackPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"m","choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":1}]}"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	}))
	defer srv.Close()
	sc := NewOpenRouter(srv.URL, "deepseek/deepseek-v4-flash", "k")
	_, usage, err := sc.Score(context.Background(), "q", []Passage{{ID: 1, Text: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if usage.CostUSD != Prices["deepseek/deepseek-v4-flash"][0] {
		t.Fatalf("cost = %v", usage.CostUSD)
	}
}

func TestScoreErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad-json":
			io.WriteString(w, `{"choices":[{"message":{"content":"not json"}}]}`)
		case "/partial":
			io.WriteString(w, `{"choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":0.5}]}"}}]}`)
		case "/duplicate":
			io.WriteString(w, `{"choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":0.5},{\"id\":1,\"score\":0.7}]}"}}]}`)
		case "/range":
			io.WriteString(w, `{"choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":1.5},{\"id\":2,\"score\":0.1}]}"}}]}`)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
			io.WriteString(w, `{"choices":[{"message":{"content":"{\"scores\":[{\"id\":1,\"score\":1}]}"}}]}`)
		default:
			w.WriteHeader(http.StatusPaymentRequired)
			io.WriteString(w, `{"error":{"message":"Insufficient credits"}}`)
		}
	}))
	defer srv.Close()
	sc := NewOpenRouter(srv.URL, "m", "secret-key")
	ps := []Passage{{ID: 1, Text: "a"}, {ID: 2, Text: "b"}}
	for _, path := range []string{"/bad-json", "/partial", "/duplicate", "/range", "/quota"} {
		if _, _, err := sc.Score(context.Background(), "q", ps); err == nil {
			t.Fatalf("%s: expected an error", path)
		} else if strings.Contains(err.Error(), "secret-key") {
			t.Fatalf("%s: error leaked the key", path)
		}
	}
	if _, _, err := sc.Score(context.Background(), "q", ps); err == nil {
		t.Fatal("quota: expected an error")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	sc.Endpoint = srv.URL + "/slow"
	if _, _, err := sc.Score(ctx, "q", ps); err == nil {
		t.Fatal("timeout: expected an error")
	}
}

func TestParseScoresFence(t *testing.T) {
	scores, err := ParseScores("```json\n{\"scores\":[{\"id\":1,\"score\":0},{\"id\":2,\"score\":1}]}\n```", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 || scores[0].Value != 0 || scores[1].Value != 1 {
		t.Fatalf("scores = %+v", scores)
	}
}
