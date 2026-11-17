package cli

import (
	"strings"
	"testing"

	"reasonix/internal/event"
)

func TestAgentReplyPaintsIncomingLetter(t *testing.T) {
	m := newTestChatTUI()
	m.ingestToolResult(event.Event{Kind: event.ToolResult, Tool: event.Tool{
		Name:   "send_message",
		Output: "Arthur: жив\nи тут",
	}})
	var found bool
	for _, source := range m.transcriptSources {
		if source.kind == transcriptSourceIncoming && source.aux == "Arthur" && source.raw == "жив\nи тут" {
			found = true
			rendered := m.renderTranscriptSource(source, m.width)
			if !strings.Contains(rendered, "← Arthur:") || !strings.Contains(rendered, "жив") {
				t.Fatalf("rendered = %q", rendered)
			}
		}
	}
	if !found {
		t.Fatalf("sources = %+v", m.transcriptSources)
	}
}

func TestPeopleReceiptStaysSilent(t *testing.T) {
	m := newTestChatTUI()
	m.ingestToolResult(event.Event{Kind: event.ToolResult, Tool: event.Tool{
		Name:   "send_message",
		Output: "ушло grok",
	}})
	for _, source := range m.transcriptSources {
		if source.kind == transcriptSourceIncoming {
			t.Fatalf("people receipt painted: %+v", source)
		}
	}
}
