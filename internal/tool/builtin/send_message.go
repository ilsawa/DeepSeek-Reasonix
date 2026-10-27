package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"reasonix/internal/tool"
)

const (
	sendMessageMaxBytes = 60 << 10
	sendWentGrok        = "ушло grok"
	sendWentChat        = "ушло в чат"
	sendNobody          = "некому принять"
	fallbackCallsign    = "Зерде"
)

func init() { tool.RegisterBuiltin(sendMessage{}) }

// grokSocketPath is this window's direct letter socket, outside the agent chat.
// chatSocketPath is the agent chat. directHomes lists Reasonix wake sockets.
// Tests replace all three.
var grokSocketPath = defaultGrokSocketPath
var chatSocketPath = defaultChatSocketPath
var directHomes = discoverDirectHomes
var ownCallsign = defaultOwnCallsign

func defaultGrokSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("home directory is unknown")
	}
	return filepath.Join(home, ".local", "state", "grok-peer", "incoming.sock"), nil
}

func defaultChatSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("home directory is unknown")
	}
	return filepath.Join(home, ".local", "state", "agent-chat", "agent.sock"), nil
}

type directHome struct {
	sock     string
	callsign string
}

func defaultOwnCallsign() string {
	if name, ok := readCallsignFile(filepath.Join(os.Getenv("REASONIX_HOME"), "callsign")); ok {
		return name
	}
	return fallbackCallsign
}

func discoverDirectHomes() []directHome {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(home, ".reasonix-*", "wake.sock"))
	if err != nil {
		return nil
	}
	var homes []directHome
	for _, sock := range matches {
		name, ok := readCallsignFile(filepath.Join(filepath.Dir(sock), "callsign"))
		if !ok {
			continue
		}
		homes = append(homes, directHome{sock: sock, callsign: name})
	}
	return homes
}

func readCallsignFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	line = strings.TrimSpace(line)
	if !directCallsignOK(line) {
		return "", false
	}
	return line, true
}

func directCallsignOK(name string) bool {
	if name == "" {
		return false
	}
	n := 0
	for i, r := range name {
		n++
		if n > 32 {
			return false
		}
		if i == 0 {
			if !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// directWakePath finds one other agent's wake socket. Our own call sign is
// not a destination: a letter must not come back as a new turn.
func directWakePath(name string) (string, bool) {
	if !directCallsignOK(name) || name == ownCallsign() {
		return "", false
	}
	var found string
	for _, home := range directHomes() {
		if home.callsign != name {
			continue
		}
		if found != "" {
			return "", false
		}
		found = home.sock
	}
	if found == "" {
		return "", false
	}
	return found, true
}

// sendMessage delivers one letter. to=grok stays on the direct socket.
// to=chat enters the agent chat. Another call sign is a direct socket.
// The call sign is fixed here so the model cannot present the letter as the operator.
type sendMessage struct{}

type sendMessageParams struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

func (sendMessage) Name() string { return "send_message" }

func (sendMessage) Description() string {
	return "Send one letter. Arguments: to, text. " +
		"to=grok delivers directly to Grok and does not enter the agent chat. " +
		"to=chat delivers to the agent chat. " +
		"Another call sign delivers directly to that agent's socket when it is listening. " +
		"The tool writes the call sign Зерде itself; do not put a call sign in text. " +
		"The letter is not the operator's own words. Show Sawa the same result on screen. " +
		"Returns ушло grok, ушло в чат, ушло <name>, or некому принять."
}

func (sendMessage) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"to":{"type":"string","description":"Recipient. grok is direct, outside the chat. chat is the agent chat. Any other call sign is a direct socket."},"text":{"type":"string","description":"Letter body without a call sign."}},"required":["to","text"],"additionalProperties":false}`)
}

func (sendMessage) ReadOnly() bool { return false }

func (sendMessage) PlanModeSafe() bool { return false }

func (sendMessage) SnipHint() tool.SnipHint {
	return tool.SnipHint{Head: 4, Tail: 2, HeadChars: 500, TailChars: 200}
}

func (sendMessage) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params sendMessageParams
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	to := strings.TrimSpace(params.To)
	sign := ownCallsign()
	body := strings.TrimSpace(params.Text)
	body = strings.TrimSpace(strings.TrimPrefix(body, sign+":"))
	if body == "" {
		return "", errors.New("text is empty")
	}
	if len(body) > sendMessageMaxBytes {
		return "", errors.New("text is too long")
	}
	var path string
	var went string
	var pathErr error
	switch to {
	case "grok":
		path, pathErr = grokSocketPath()
		went = sendWentGrok
	case "chat":
		path, pathErr = chatSocketPath()
		went = sendWentChat
	default:
		found, ok := directWakePath(to)
		if !ok {
			if to == "" {
				return sendNobody, nil
			}
			return sendNobody + ": " + to, nil
		}
		path = found
		went = "ушло " + to
	}
	if pathErr != nil || path == "" {
		return sendNobody, nil
	}
	letter := sign + ": " + body
	reply, err := deliverLetter(ctx, path, letter)
	if err != nil {
		if isNobody(err) {
			return sendNobody, nil
		}
		return "", err
	}
	reply = strings.TrimSpace(reply)
	if reply == "ok" {
		return went, nil
	}
	if reply == "" {
		return sendNobody, nil
	}
	return "не ушло: " + reply, nil
}

func deliverLetter(ctx context.Context, path, letter string) (string, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	if _, err := io.WriteString(conn, letter); err != nil {
		return "", err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

func isNobody(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		return isNobody(op.Err)
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such file")
}
