package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	agentTalkTimeout     = 3 * time.Minute
	agentHistoryMessages = 40
	agentHistoryChars    = 64000
	defaultAgentURL      = "http://127.0.0.1:8080"
)

// agentTalkMu keeps one agent's thread in order. A reply can take the whole
// timeout, and a second letter must see the first answer.
var agentTalkMu sync.Mutex

// agentHTTP is replaced by tests that need a custom transport.
var agentHTTP = http.DefaultClient

type agentTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type agentThreadFile struct {
	Messages []agentTurn `json:"messages"`
}

// parseAgentDest reports an agent destination. ok is true when the prefix
// matches, even if the name is illegal: that must not fall through to a person.
func parseAgentDest(to string) (name string, fresh bool, ok bool) {
	const freshPrefix = "agent-new:"
	const prefix = "agent:"
	switch {
	case strings.HasPrefix(to, freshPrefix):
		return strings.TrimSpace(to[len(freshPrefix):]), true, true
	case strings.HasPrefix(to, prefix):
		return strings.TrimSpace(to[len(prefix):]), false, true
	default:
		return "", false, false
	}
}

// agentNameOK is the screen sender: a letter, then letters, digits, hyphen
// or underscore, at most 32. The same rule paints the yellow line.
func agentNameOK(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 32 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && (r == '-' || r == '_' || (r >= '0' && r <= '9')):
		default:
			return false
		}
	}
	return true
}

func talkToAgent(ctx context.Context, name string, fresh bool, text string) (string, error) {
	if !agentNameOK(name) {
		shown := name
		if utf8.RuneCountInString(shown) > 40 {
			shown = string([]rune(shown)[:40])
		}
		return "", fmt.Errorf("имя агента не годится: %s", shown)
	}
	agentTalkMu.Lock()
	defer agentTalkMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, agentTalkTimeout)
	defer cancel()

	path, err := agentThreadPath(name)
	if err != nil {
		return "", err
	}
	var history []agentTurn
	if fresh {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	} else {
		history, err = loadAgentThread(path)
		if err != nil {
			return "", err
		}
		history = trimAgentThread(history)
	}
	reply, err := askLocalAIAgent(ctx, name, text, history)
	if err != nil {
		return "", err
	}
	stored := append(append([]agentTurn{}, history...),
		agentTurn{Role: "user", Content: text},
		agentTurn{Role: "assistant", Content: reply},
	)
	if err := saveAgentThread(path, stored); err != nil {
		return "", err
	}
	return name + ": " + reply, nil
}

func agentThreadPath(name string) (string, error) {
	home := os.Getenv("REASONIX_HOME")
	if home == "" {
		return "", errors.New("REASONIX_HOME is empty")
	}
	return filepath.Join(home, "localai-agent-threads", name+".json"), nil
}

func loadAgentThread(path string) ([]agentTurn, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc agentThreadFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("нить агента не читается: %w", err)
	}
	kept := make([]agentTurn, 0, len(doc.Messages))
	for _, m := range doc.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role != "user" && role != "assistant" {
			continue
		}
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		kept = append(kept, agentTurn{Role: role, Content: m.Content})
	}
	return kept, nil
}

func saveAgentThread(path string, messages []agentTurn) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	doc := agentThreadFile{Messages: trimAgentThread(messages)}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// trimAgentThread keeps the newest turns, matching the LocalAI chat cap:
// 40 messages, then the newest run that still fits in 64000 characters.
func trimAgentThread(messages []agentTurn) []agentTurn {
	if len(messages) > agentHistoryMessages {
		messages = messages[len(messages)-agentHistoryMessages:]
	}
	total := 0
	start := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		total += len(messages[i].Content)
		if total > agentHistoryChars {
			break
		}
		start = i
	}
	return append([]agentTurn(nil), messages[start:]...)
}

func askLocalAIAgent(ctx context.Context, name, text string, history []agentTurn) (string, error) {
	key, err := localAIAgentKey()
	if err != nil {
		return "", err
	}
	base := localAIAgentURL()
	sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/agents/"+name+"/sse", nil)
	if err != nil {
		return "", err
	}
	sseReq.Header.Set("Authorization", "Bearer "+key)
	sseReq.Header.Set("Accept", "text/event-stream")
	sseResp, err := agentHTTP.Do(sseReq)
	if err != nil {
		return "", fmt.Errorf("%s не отвечает: %w", name, err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s не найден", name)
	}
	if sseResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s не отвечает: sse %d", name, sseResp.StatusCode)
	}

	replyCh := make(chan agentRead, 1)
	go func() {
		content, readErr := readAgentSSE(sseResp.Body)
		replyCh <- agentRead{content: content, err: readErr}
	}()

	payload, err := json.Marshal(struct {
		Message string      `json:"message"`
		History []agentTurn `json:"history,omitempty"`
	}{Message: text, History: history})
	if err != nil {
		return "", err
	}
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/agents/"+name+"/chat", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	postReq.Header.Set("Authorization", "Bearer "+key)
	postReq.Header.Set("Content-Type", "application/json")
	postResp, err := agentHTTP.Do(postReq)
	if err != nil {
		return "", fmt.Errorf("%s не отвечает: %w", name, err)
	}
	defer postResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(postResp.Body, 4096))
	if postResp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s не найден", name)
	}
	if postResp.StatusCode != http.StatusAccepted && postResp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s не принял письмо: %d", name, postResp.StatusCode)
	}

	select {
	case got := <-replyCh:
		if got.err != nil {
			return "", fmt.Errorf("%s не отвечает: %w", name, got.err)
		}
		if strings.TrimSpace(got.content) == "" {
			return "", fmt.Errorf("%s прислал пустой ответ", name)
		}
		return got.content, nil
	case <-ctx.Done():
		return "", fmt.Errorf("%s не отвечает: %w", name, ctx.Err())
	}
}

type agentRead struct {
	content string
	err     error
}

func readAgentSSE(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	var event string
	var data strings.Builder
	flush := func() (string, error, bool) {
		ev := event
		payload := data.String()
		event = ""
		data.Reset()
		if payload == "" {
			return "", nil, false
		}
		switch ev {
		case "", "json_message":
			var msg struct {
				Sender  string `json:"sender"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal([]byte(payload), &msg); err != nil {
				return "", nil, false
			}
			if msg.Sender == "agent" && strings.TrimSpace(msg.Content) != "" {
				return msg.Content, nil, true
			}
		case "json_error":
			var msg struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &msg); err != nil || msg.Error == "" {
				return "", errors.New("ошибка агента"), true
			}
			return "", errors.New(msg.Error), true
		}
		return "", nil, false
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			if content, err, done := flush(); done {
				return content, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return "", err
	}
	if content, err, done := flush(); done {
		return content, err
	}
	return "", errors.New("ответ не пришёл")
}

func localAIAgentURL() string {
	if u := strings.TrimRight(strings.TrimSpace(os.Getenv("LOCALAI_AGENT_URL")), "/"); u != "" {
		return u
	}
	return defaultAgentURL
}

func localAIAgentKey() (string, error) {
	if k := strings.TrimSpace(os.Getenv("LOCALAI_AGENT_KEY")); k != "" {
		return k, nil
	}
	path := strings.TrimSpace(os.Getenv("LOCALAI_AGENT_KEY_FILE"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", errors.New("нет ключа LocalAI")
		}
		path = filepath.Join(home, ".config", "localai", "accounts", "iskin.env")
	}
	return keyFromEnvFile(path)
}

func keyFromEnvFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("нет ключа LocalAI")
	}
	defer f.Close()
	var local, api string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "LOCALAI_KEY":
			local = v
		case "api_key":
			api = v
		}
	}
	if err := sc.Err(); err != nil {
		return "", errors.New("нет ключа LocalAI")
	}
	if local != "" {
		return local, nil
	}
	if api != "" {
		return api, nil
	}
	return "", errors.New("нет ключа LocalAI")
}
