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
	// agentHistoryChars is a byte cap (len of the content string). The studio
	// chat is the transcript the model sees. This file is only a mirror, so a
	// long answer is kept instead of being cut at 64 KiB.
	agentHistoryChars = 1 << 20
	defaultAgentURL   = "http://127.0.0.1:8080"
)

// agentPollEvery is how often a letter waits for the studio turn to finish.
// Tests shorten it.
var agentPollEvery = 350 * time.Millisecond

// agentTalkMu keeps one agent's thread in order. A reply can take the whole
// timeout, and a second letter must see the first answer.
var agentTalkMu sync.Mutex

// agentHTTP is replaced by tests that need a custom transport.
var agentHTTP = http.DefaultClient

// knightContext is the upstream window, in tokens, that the studio chat plans
// with. These models are cloud proxies, so the number is a budget, not a local
// cache. A name that is not here keeps the window the chat already has.
var knightContext = map[string]int{
	"Arthur":   131072,
	"Bedivere": 131072,
	"Bors":     131072,
	"Gawain":   131072,
	"Kay":      16384,
	"Lancelot": 2097152,
	"Percival": 2097152,
}

// knightToolsOff are the built-in groups a knight chat starts with switched
// off. An empty list on the saved agent means every default group is on, and
// those tools spend the window and change the letter. Opt-in groups are
// already off and are not listed here.
var knightToolsOff = []string{
	"web", "triggers", "shell", "files", "media", "collections",
	"changes", "projects", "memory", "subagents", "localai_client",
}

type agentTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type agentThreadFile struct {
	ChatID   string      `json:"chat_id,omitempty"`
	Messages []agentTurn `json:"messages"`
}

type studioAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type studioChat struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	AgentID    string          `json:"agent_id"`
	Running    bool            `json:"running"`
	WillResume bool            `json:"will_resume"`
	Settings   json.RawMessage `json:"settings"`
}

type studioMessage struct {
	Seq  int64  `json:"seq"`
	Role string `json:"role"`
	Text string `json:"text"`
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
	chatID, history, err := loadAgentThread(path)
	if err != nil {
		return "", err
	}
	if fresh {
		// The studio chat is cleared below. Drop the mirror, keep the chat id
		// so the next letter does not open a second chat.
		history = nil
	} else {
		history = trimAgentThread(history)
	}
	reply, chatID, err := askLocalAIAgent(ctx, name, text, chatID, fresh)
	if err != nil {
		return "", err
	}
	stored := append(append([]agentTurn{}, history...),
		agentTurn{Role: "user", Content: text},
		agentTurn{Role: "assistant", Content: reply},
	)
	if err := saveAgentThread(path, chatID, stored); err != nil {
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

func loadAgentThread(path string) (string, []agentTurn, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var doc agentThreadFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", nil, fmt.Errorf("нить агента не читается: %w", err)
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
	return strings.TrimSpace(doc.ChatID), kept, nil
}

func saveAgentThread(path, chatID string, messages []agentTurn) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	doc := agentThreadFile{ChatID: chatID, Messages: trimAgentThread(messages)}
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

// trimAgentThread keeps the newest turns of the local mirror: 40 messages,
// then the newest run that still fits in agentHistoryChars bytes.
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

func askLocalAIAgent(ctx context.Context, name, text, chatID string, fresh bool) (string, string, error) {
	agent, err := findStudioAgent(ctx, name)
	if err != nil {
		return "", "", err
	}
	chat, err := knightChat(ctx, name, agent.ID, chatID)
	if err != nil {
		return "", "", err
	}
	chat, err = ensureKnightContext(ctx, name, chat)
	if err != nil {
		return "", "", err
	}
	if fresh {
		if err := clearKnightChat(ctx, name, chat.ID); err != nil {
			return "", "", err
		}
	}
	after, err := lastMessageSeq(ctx, name, chat.ID)
	if err != nil {
		return "", "", err
	}
	if err := postKnightText(ctx, name, chat.ID, text); err != nil {
		return "", "", err
	}
	reply, err := waitKnightReply(ctx, name, chat.ID, text, after)
	if err != nil {
		return "", "", err
	}
	return reply, chat.ID, nil
}

func findStudioAgent(ctx context.Context, name string) (studioAgent, error) {
	var list struct {
		Agents []studioAgent `json:"agents"`
	}
	status, err := agentJSON(ctx, http.MethodGet, "/api/chats/agents", nil, &list)
	if err != nil {
		return studioAgent{}, fmt.Errorf("%s не отвечает: %w", name, err)
	}
	if status != http.StatusOK {
		return studioAgent{}, agentStatus(name, status, nil)
	}
	for _, a := range list.Agents {
		if a.Name == name && a.ID != "" {
			return a, nil
		}
	}
	return studioAgent{}, fmt.Errorf("%s не найден", name)
}

// knightChat reuses the one studio chat of this agent. A stored id wins when
// it still belongs to the agent. Otherwise the single listed chat is used,
// and when several exist the one titled with the agent's name wins. A second
// chat is opened only when the agent has none.
func knightChat(ctx context.Context, name, agentID, stored string) (studioChat, error) {
	if stored != "" {
		chat, status, err := getKnightChat(ctx, name, stored)
		if err != nil {
			return studioChat{}, err
		}
		if status == http.StatusOK && chat.AgentID == agentID {
			return chat, nil
		}
		if status != http.StatusNotFound && status != http.StatusOK {
			return studioChat{}, agentStatus(name, status, nil)
		}
	}
	var list struct {
		Conversations []studioChat `json:"conversations"`
	}
	status, err := agentJSON(ctx, http.MethodGet, "/api/chats?limit=200&kind=chat", nil, &list)
	if err != nil {
		return studioChat{}, fmt.Errorf("%s не отвечает: %w", name, err)
	}
	if status != http.StatusOK {
		return studioChat{}, agentStatus(name, status, nil)
	}
	var match []studioChat
	for _, c := range list.Conversations {
		if c.AgentID == agentID && c.ID != "" {
			match = append(match, c)
		}
	}
	if len(match) > 1 {
		var titled []studioChat
		for _, c := range match {
			if c.Title == name {
				titled = append(titled, c)
			}
		}
		if len(titled) > 0 {
			match = titled
		}
	}
	if len(match) > 0 {
		return match[0], nil
	}
	var created studioChat
	body := map[string]any{
		"agent_id": agentID,
		"title":    name,
		"switches": map[string]any{"builtins_off": knightToolsOff},
	}
	status, err = agentJSON(ctx, http.MethodPost, "/api/chats", body, &created)
	if err != nil {
		return studioChat{}, fmt.Errorf("%s не отвечает: %w", name, err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return studioChat{}, agentStatus(name, status, nil)
	}
	if created.ID == "" {
		return studioChat{}, fmt.Errorf("%s не отвечает: чат не создан", name)
	}
	return created, nil
}

func getKnightChat(ctx context.Context, name, id string) (studioChat, int, error) {
	var chat studioChat
	status, err := agentJSON(ctx, http.MethodGet, "/api/chats/"+id, nil, &chat)
	if err != nil {
		return studioChat{}, 0, fmt.Errorf("%s не отвечает: %w", name, err)
	}
	return chat, status, nil
}

func ensureKnightContext(ctx context.Context, name string, chat studioChat) (studioChat, error) {
	want := knightContext[name]
	if want == 0 || settingContext(chat.Settings) == want {
		return chat, nil
	}
	settings := map[string]any{}
	if len(chat.Settings) > 0 && string(chat.Settings) != "null" {
		if err := json.Unmarshal(chat.Settings, &settings); err != nil {
			return studioChat{}, fmt.Errorf("%s не отвечает: %w", name, err)
		}
	}
	settings["context_size"] = want
	var updated studioChat
	status, err := agentJSON(ctx, http.MethodPatch, "/api/chats/"+chat.ID, map[string]any{"settings": settings}, &updated)
	if err != nil {
		return studioChat{}, fmt.Errorf("%s не отвечает: %w", name, err)
	}
	if status != http.StatusOK {
		return studioChat{}, agentStatus(name, status, nil)
	}
	if updated.ID == "" {
		updated.ID = chat.ID
	}
	return updated, nil
}

func settingContext(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var s struct {
		ContextSize int `json:"context_size"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0
	}
	return s.ContextSize
}

func clearKnightChat(ctx context.Context, name, id string) error {
	status, raw, err := agentRequest(ctx, http.MethodPost, "/api/chats/"+id+"/clear", nil)
	if err != nil {
		return fmt.Errorf("%s не отвечает: %w", name, err)
	}
	// 400 is an empty transcript: there is nothing above the marker to drop.
	if status == http.StatusOK || status == http.StatusBadRequest {
		return nil
	}
	return agentStatus(name, status, raw)
}

func lastMessageSeq(ctx context.Context, name, id string) (int64, error) {
	msgs, err := knightMessages(ctx, name, id, 0)
	if err != nil {
		return 0, err
	}
	var last int64
	for _, m := range msgs {
		if m.Seq > last {
			last = m.Seq
		}
	}
	return last, nil
}

func knightMessages(ctx context.Context, name, id string, after int64) ([]studioMessage, error) {
	var all []studioMessage
	for {
		var page struct {
			Messages []studioMessage `json:"messages"`
		}
		path := fmt.Sprintf("/api/chats/%s/messages?limit=1000&after=%d", id, after)
		status, err := agentJSON(ctx, http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, fmt.Errorf("%s не отвечает: %w", name, err)
		}
		if status != http.StatusOK {
			return nil, agentStatus(name, status, nil)
		}
		all = append(all, page.Messages...)
		if len(page.Messages) < 1000 {
			return all, nil
		}
		after = page.Messages[len(page.Messages)-1].Seq
	}
}

func postKnightText(ctx context.Context, name, id, text string) error {
	status, raw, err := agentRequest(ctx, http.MethodPost, "/api/chats/"+id+"/messages", map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("%s не отвечает: %w", name, err)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("%s не найден", name)
	}
	if status != http.StatusOK {
		return agentStatus(name, status, raw)
	}
	return nil
}

// waitKnightReply reads the transcript until the turn is idle and the new
// user line has an assistant answer or a failure after it. A partial assistant
// row is written while the turn still runs, so an idle chat is required.
func waitKnightReply(ctx context.Context, name, id, text string, after int64) (string, error) {
	seenBusy := false
	quiet := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%s не отвечает: %w", name, err)
		}
		chat, status, err := getKnightChat(ctx, name, id)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", agentStatus(name, status, nil)
		}
		msgs, err := knightMessages(ctx, name, id, after)
		if err != nil {
			return "", err
		}
		reply, failure, userSeen := takeKnightReply(msgs, text, after)
		busy := chat.Running || chat.WillResume
		if !busy && userSeen {
			if failure != "" {
				return "", fmt.Errorf("%s: %s", name, failure)
			}
			if reply != "" {
				return reply, nil
			}
			quiet++
			if seenBusy || quiet >= 3 {
				return "", fmt.Errorf("%s прислал пустой ответ", name)
			}
		} else {
			quiet = 0
		}
		if busy {
			seenBusy = true
		}
		if err := waitAgent(ctx); err != nil {
			return "", fmt.Errorf("%s не отвечает: %w", name, err)
		}
	}
}

func takeKnightReply(messages []studioMessage, text string, after int64) (reply, failure string, userSeen bool) {
	userSeq := int64(-1)
	for _, m := range messages {
		if m.Seq <= after || m.Role != "user" || m.Text != text {
			continue
		}
		userSeq = m.Seq
	}
	if userSeq < 0 {
		return "", "", false
	}
	for _, m := range messages {
		if m.Seq <= userSeq || strings.TrimSpace(m.Text) == "" {
			continue
		}
		switch m.Role {
		case "failure":
			failure = m.Text
		case "assistant":
			reply = m.Text
		}
	}
	if failure != "" {
		return "", failure, true
	}
	return reply, "", true
}

func waitAgent(ctx context.Context) error {
	timer := time.NewTimer(agentPollEvery)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func agentJSON(ctx context.Context, method, path string, body any, dest any) (int, error) {
	status, raw, err := agentRequest(ctx, method, path, body)
	if err != nil || dest == nil || len(raw) == 0 {
		return status, err
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return status, err
	}
	return status, nil
}

func agentRequest(ctx context.Context, method, path string, body any) (int, []byte, error) {
	key, err := localAIAgentKey()
	if err != nil {
		return 0, nil, err
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, localAIAgentURL()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := agentHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

func agentStatus(name string, status int, raw []byte) error {
	if status == http.StatusNotFound {
		return fmt.Errorf("%s не найден", name)
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := strings.TrimSpace(body.Error.Message)
	if msg == "" {
		return fmt.Errorf("%s не отвечает: %d", name, status)
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return fmt.Errorf("%s не отвечает: %s", name, msg)
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
