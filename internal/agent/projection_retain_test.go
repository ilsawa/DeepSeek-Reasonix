package agent

import (
	"testing"

	"reasonix/internal/agent/testutil"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func TestRetainCoveredCanonicalKeepsFoldPrefix(t *testing.T) {
	canonical := []provider.Message{
		{ID: "system", Role: provider.RoleSystem, Content: "system"},
		{ID: "old-user", Role: provider.RoleUser, Content: "old question"},
		{ID: "old-answer", Role: provider.RoleAssistant, Content: "old answer"},
		{ID: "partial", Role: provider.RoleAssistant, Content: "unfinished"},
	}
	sess := &Session{Messages: append([]provider.Message(nil), canonical...)}
	a := New(testutil.NewMock("test"), tool.NewRegistry(), sess, Options{}, event.Discard)
	covered := len(canonical)
	a.sess.compactionMu.Lock()
	a.sess.compactionState = CompactionState{
		SchemaVersion:  compactionStateSchemaCurrent,
		PromptCacheKey: a.currentPromptCacheKeyLocked(),
		Projection: ContextProjection{
			Messages: []provider.Message{
				canonical[0],
				formatSummaryMessage("earlier work"),
			},
			ProjectionVersion: 1,
			CoveredCount:      covered,
			CoveredPrefixHash: coveredPrefixHash(canonical, covered),
			PinnedContextHash: pinnedContextCoverageHash(canonical, covered),
		},
	}
	a.sess.compactionMu.Unlock()

	stripped := canonical[:3]
	got := a.RetainCoveredCanonical(canonical, stripped)
	if len(got) != len(canonical) || got[len(got)-1].ID != "partial" {
		t.Fatalf("covered prefix was cut: %d messages, last=%s", len(got), messageID(got))
	}
	if visible := a.modelVisibleMessages(); !hasCompactionSummary(visible) {
		t.Fatal("projection was not visible before retain")
	}
	a.Session().Replace(got)
	if visible := a.modelVisibleMessages(); !hasCompactionSummary(visible) {
		t.Fatal("keeping the covered prefix invalidated the fold")
	}
	a.Session().Replace(stripped)
	if visible := a.modelVisibleMessages(); hasCompactionSummary(visible) {
		t.Fatal("cutting the covered prefix still projected")
	}

	a.InvalidateProjection()
	if got := a.RetainCoveredCanonical(canonical, stripped); len(got) != len(stripped) {
		t.Fatalf("missing projection changed the cut: %d", len(got))
	}
}

func messageID(messages []provider.Message) string {
	if len(messages) == 0 {
		return ""
	}
	return messages[len(messages)-1].ID
}
