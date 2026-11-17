package cli

import (
	"strings"
	"unicode/utf8"

	"reasonix/internal/event"
)

// agentReplyFromTool recognises a LocalAI agent answer from send_message.
// People receipts (ушло grok, ушло в чат) do not match, so they stay silent.
func agentReplyFromTool(tool event.Tool) (sender, body string, ok bool) {
	if tool.Name != "send_message" || tool.Err != "" {
		return "", "", false
	}
	return parseAgentReply(tool.Output)
}

// parseAgentReply reads "Name: text". Name is the same screen sender the
// agent door accepts: a letter, then letters, digits, hyphen or underscore.
func parseAgentReply(output string) (sender, body string, ok bool) {
	line, rest, found := strings.Cut(output, "\n")
	name, text, cut := strings.Cut(line, ": ")
	if !cut || !agentSenderOK(name) {
		return "", "", false
	}
	body = text
	if found {
		if body != "" {
			body += "\n"
		}
		body += rest
	}
	if strings.TrimSpace(body) == "" {
		return "", "", false
	}
	return name, body, true
}

func agentSenderOK(name string) bool {
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
