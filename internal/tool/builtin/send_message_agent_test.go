package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSendMessageAgentKeepsThreadAndSkipsPeople(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REASONIX_HOME", dir)
	t.Setenv("LOCALAI_AGENT_KEY", "test-key")
	agentPollEvery = time.Millisecond
	t.Cleanup(func() { agentPollEvery = 350 * time.Millisecond })

	grokPath := filepath.Join(dir, "incoming.sock")
	restore := grokSocketPath
	grokSocketPath = func() (string, error) { return grokPath, nil }
	t.Cleanup(func() { grokSocketPath = restore })
	grok := listenLetters(t, grokPath)

	var mu sync.Mutex
	var saw []studioCall
	type line struct {
		seq  int64
		role string
		text string
	}
	var (
		chatID   = "chat-arthur"
		created  int
		clears   int
		messages []line
		settings map[string]any
		seq      int64
		wait     int
	)
	record := func(r *http.Request, extra string) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		saw = append(saw, studioCall{method: r.Method, path: r.URL.RequestURI(), body: string(body), extra: extra})
		mu.Unlock()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/chats/agents":
			record(r, "")
			_, _ = io.WriteString(w, `{"agents":[{"id":"ag-arthur","name":"Arthur"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/chats" && r.URL.Query().Get("kind") == "chat":
			record(r, "")
			if created == 0 {
				_, _ = io.WriteString(w, `{"conversations":[]}`)
				return
			}
			_, _ = io.WriteString(w, `{"conversations":[{"id":"`+chatID+`","title":"Arthur","agent_id":"ag-arthur"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/chats":
			var body struct {
				AgentID  string `json:"agent_id"`
				Title    string `json:"title"`
				Switches struct {
					Off []string `json:"builtins_off"`
				} `json:"switches"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			saw = append(saw, studioCall{method: r.Method, path: r.URL.Path, body: string(raw)})
			created++
			mu.Unlock()
			if body.AgentID != "ag-arthur" || body.Title != "Arthur" || !stringListHas(body.Switches.Off, "shell") {
				http.Error(w, "bad create", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"`+chatID+`","title":"Arthur","agent_id":"ag-arthur"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/chats/"+chatID:
			record(r, "")
			running := false
			if wait == 1 {
				running = true
				wait = 2
			} else if wait == 2 {
				for i := range messages {
					if messages[i].role == "assistant" && messages[i].text == "об" {
						messages[i].text = "ответ рыцаря"
					}
				}
				wait = 0
			}
			raw, _ := json.Marshal(settings)
			if settings == nil {
				raw = []byte("null")
			}
			_, _ = io.WriteString(w, `{"id":"`+chatID+`","title":"Arthur","agent_id":"ag-arthur","running":`+boolJSON(running)+`,"settings":`+string(raw)+`}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/chats/"+chatID:
			var body struct {
				Settings map[string]any `json:"settings"`
			}
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			saw = append(saw, studioCall{method: r.Method, path: r.URL.Path, body: string(raw)})
			settings = body.Settings
			mu.Unlock()
			ctx, _ := body.Settings["context_size"].(float64)
			_, _ = io.WriteString(w, `{"id":"`+chatID+`","title":"Arthur","agent_id":"ag-arthur","running":false,"settings":{"context_size":`+strconv.Itoa(int(ctx))+`}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/chats/"+chatID+"/clear":
			record(r, "")
			clears++
			if len(messages) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"nothing to clear"}}`)
				return
			}
			messages = nil
			_, _ = io.WriteString(w, `{"cleared":true}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/chats/"+chatID+"/messages"):
			record(r, "")
			out := struct {
				Messages []studioMessage `json:"messages"`
			}{}
			for _, m := range messages {
				out.Messages = append(out.Messages, studioMessage{Seq: m.seq, Role: m.role, Text: m.text})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/api/chats/"+chatID+"/messages":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if _, ok := body["history"]; ok || body["message"] != nil {
				http.Error(w, "history replayed", http.StatusBadRequest)
				return
			}
			text, _ := body["text"].(string)
			mu.Lock()
			saw = append(saw, studioCall{method: r.Method, path: r.URL.Path, body: string(raw)})
			seq++
			messages = append(messages, line{seq: seq, role: "user", text: text})
			seq++
			messages = append(messages, line{seq: seq, role: "assistant", text: "об"})
			wait = 1
			mu.Unlock()
			_, _ = io.WriteString(w, `{"started":true}`)
		default:
			record(r, "unexpected")
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("LOCALAI_AGENT_URL", srv.URL)

	out, err := sendMessage{}.Execute(context.Background(), []byte(`{"to":"agent:Arthur","text":"первый"}`))
	if err != nil || out != "Arthur: ответ рыцаря" {
		t.Fatalf("first = %q %v", out, err)
	}
	out, err = sendMessage{}.Execute(context.Background(), []byte(`{"to":"agent:Arthur","text":"второй"}`))
	if err != nil || out != "Arthur: ответ рыцаря" {
		t.Fatalf("second = %q %v", out, err)
	}
	out, err = sendMessage{}.Execute(context.Background(), []byte(`{"to":"agent-new:Arthur","text":"заново"}`))
	if err != nil || out != "Arthur: ответ рыцаря" {
		t.Fatalf("fresh = %q %v", out, err)
	}
	if got := grok.takeNone(); got != "" {
		t.Fatalf("agent letter reached grok: %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if created != 1 {
		t.Fatalf("created = %d", created)
	}
	if clears != 1 {
		t.Fatalf("clears = %d", clears)
	}
	var posts []string
	var chatOfPost string
	patches := 0
	for _, c := range saw {
		if strings.Contains(c.path, "/api/agents/") {
			t.Fatalf("old agent route: %+v", c)
		}
		if c.method == http.MethodPost && strings.HasSuffix(c.path, "/messages") {
			posts = append(posts, c.body)
			if chatOfPost == "" {
				chatOfPost = c.path
			} else if c.path != chatOfPost {
				t.Fatalf("chat changed: %s then %s", chatOfPost, c.path)
			}
		}
		if c.method == http.MethodPatch {
			patches++
			if !strings.Contains(c.body, `"context_size":131072`) {
				t.Fatalf("patch = %s", c.body)
			}
		}
	}
	if patches != 1 {
		t.Fatalf("patches = %d", patches)
	}
	if len(posts) != 3 || posts[0] != `{"text":"первый"}` || posts[1] != `{"text":"второй"}` || posts[2] != `{"text":"заново"}` {
		t.Fatalf("posts = %#v", posts)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "localai-agent-threads", "Arthur.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "заново") || strings.Contains(string(raw), "первый") || !strings.Contains(string(raw), chatID) {
		t.Fatalf("thread file = %s", raw)
	}
	info, err := os.Stat(filepath.Join(dir, "localai-agent-threads", "Arthur.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestSendMessageAgentRejectsBadName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REASONIX_HOME", dir)
	t.Setenv("LOCALAI_AGENT_URL", "http://127.0.0.1:1")
	t.Setenv("LOCALAI_AGENT_KEY", "test-key")
	out, err := sendMessage{}.Execute(context.Background(), []byte(`{"to":"agent:kay.dot","text":"привет"}`))
	if err == nil || out != "" || !strings.Contains(err.Error(), "не годится") {
		t.Fatalf("dotted = %q %v", out, err)
	}
	if strings.Contains(err.Error(), "некому") {
		t.Fatal("bad agent name fell through to a person")
	}
	out, err = sendMessage{}.Execute(context.Background(), []byte(`{"to":"agent:","text":"привет"}`))
	if err == nil || !strings.Contains(err.Error(), "не годится") {
		t.Fatalf("empty = %q %v", out, err)
	}
}

func TestTrimAgentThreadCaps(t *testing.T) {
	messages := make([]agentTurn, 50)
	for i := range messages {
		messages[i] = agentTurn{Role: "user", Content: "x"}
	}
	got := trimAgentThread(messages)
	if len(got) != 40 {
		t.Fatalf("len = %d", len(got))
	}
	huge := []agentTurn{{Role: "user", Content: strings.Repeat("a", agentHistoryChars+1)}, {Role: "assistant", Content: "tail"}}
	got = trimAgentThread(huge)
	if len(got) != 1 || got[0].Content != "tail" {
		t.Fatalf("char cap = %+v", got)
	}
}

func stringListHas(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

type studioCall struct {
	method string
	path   string
	body   string
	extra  string
}
