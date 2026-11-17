package builtin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSendMessageAgentKeepsThreadAndSkipsPeople(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REASONIX_HOME", dir)
	t.Setenv("LOCALAI_AGENT_KEY", "test-key")

	grokPath := filepath.Join(dir, "incoming.sock")
	restore := grokSocketPath
	grokSocketPath = func() (string, error) { return grokPath, nil }
	t.Cleanup(func() { grokSocketPath = restore })
	grok := listenLetters(t, grokPath)

	var mu sync.Mutex
	var saw []chatCall
	posted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sse"):
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "no flush", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher.Flush()
			select {
			case <-posted:
			case <-r.Context().Done():
				return
			}
			_, _ = io.WriteString(w, "event: json_message\ndata: {\"sender\":\"user\",\"content\":\"эхо\"}\n\n")
			_, _ = io.WriteString(w, "event: json_message\ndata: {\"sender\":\"agent\",\"content\":\"ответ рыцаря\"}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat"):
			var body struct {
				Message string      `json:"message"`
				History []agentTurn `json:"history"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			saw = append(saw, chatCall{message: body.Message, history: append([]agentTurn(nil), body.History...)})
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"status":"message_received","message_id":"1"}`)
			posted <- struct{}{}
		default:
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
	if len(saw) != 3 {
		t.Fatalf("calls = %d", len(saw))
	}
	if len(saw[0].history) != 0 || saw[0].message != "первый" {
		t.Fatalf("first history = %+v", saw[0])
	}
	if len(saw[1].history) != 2 || saw[1].history[0].Content != "первый" || saw[1].history[1].Role != "assistant" || saw[1].message != "второй" {
		t.Fatalf("second history = %+v", saw[1])
	}
	if len(saw[2].history) != 0 || saw[2].message != "заново" {
		t.Fatalf("fresh history = %+v", saw[2])
	}
	raw, err := os.ReadFile(filepath.Join(dir, "localai-agent-threads", "Arthur.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "заново") || strings.Contains(string(raw), "первый") {
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

type chatCall struct {
	message string
	history []agentTurn
}
