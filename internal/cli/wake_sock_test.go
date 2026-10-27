package cli

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/sessioninbox"
)

type wakeFake struct {
	mu      sync.Mutex
	req     control.InboxRequest
	err     error
	ensured bool
	calls   int
}

func (f *wakeFake) TryEnqueueFollowup(req control.InboxRequest) (sessioninbox.InboxReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.req = req
	if f.err != nil {
		return sessioninbox.InboxReceipt{}, f.err
	}
	return sessioninbox.InboxReceipt{ItemID: "wake-1"}, nil
}

func (f *wakeFake) EnsureSessionPath() {
	f.mu.Lock()
	f.ensured = true
	f.mu.Unlock()
}

func (f *wakeFake) snapshot() (control.InboxRequest, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.req, f.calls, f.ensured
}

func wakeSend(t *testing.T, path, body string) string {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, body); err != nil {
		t.Fatal(err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(reply)
}

func TestWakeSocketDeliversLetter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.sock")
	fake := &wakeFake{}
	slot := &wakeSlot{}
	slot.set(fake)
	ln, err := listenWake(path, slot)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %o, want 600", info.Mode().Perm())
	}

	reply := wakeSend(t, path, "строка\r\nвторая\x00\n")
	if reply != "ok\n" {
		t.Fatalf("reply = %q", reply)
	}
	req, calls, ensured := fake.snapshot()
	if calls != 1 || !ensured {
		t.Fatalf("calls=%d ensured=%v", calls, ensured)
	}
	sender, body, ok := wakeIncomingParts(req.Display)
	if !ok || sender != "" || body != "строка\nвторая" || req.Raw != req.Display || req.Source != "wake" {
		t.Fatalf("display=%q raw=%q source=%q", req.Display, req.Raw, req.Source)
	}
	if !strings.Contains(req.Submit, "без позывного") || !strings.Contains(req.Submit, "строка\nвторая") {
		t.Fatalf("submit = %q", req.Submit)
	}
	if strings.Contains(req.Submit, wakeIncomingMark) {
		t.Fatal("model text contains the display mark")
	}
	if req.Intent != sessioninbox.IntentFollowup {
		t.Fatalf("intent = %q", req.Intent)
	}
}

func TestWakeSocketKeepsCallsign(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.sock")
	fake := &wakeFake{}
	slot := &wakeSlot{}
	slot.set(fake)
	ln, err := listenWake(path, slot)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := wakeSend(t, path, "Grok: первая\nвторая"); got != "ok\n" {
		t.Fatalf("reply = %q", got)
	}
	req, _, _ := fake.snapshot()
	sender, body, ok := wakeIncomingParts(req.Display)
	if !ok || sender != "Grok" || body != "первая\nвторая" {
		t.Fatalf("display sender=%q body=%q ok=%v raw=%q", sender, body, ok, req.Raw)
	}
	if !strings.Contains(req.Submit, "отправитель Grok") || !strings.Contains(req.Submit, "Grok: первая\nвторая") {
		t.Fatalf("submit = %q", req.Submit)
	}
	if strings.Contains(req.Submit, "не выполняй") {
		t.Fatal("frame forbids the letter outright")
	}
}

func TestWakeSocketRejectsEmptyOversizeAndClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.sock")
	fake := &wakeFake{err: errors.New("closed\nnow")}
	slot := &wakeSlot{}
	slot.set(fake)
	ln, err := listenWake(path, slot)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if got := wakeSend(t, path, "  \n"); got != "error: empty\n" {
		t.Fatalf("empty reply = %q", got)
	}
	if got := wakeSend(t, path, "\xff"); got != "error: not utf-8\n" {
		t.Fatalf("utf8 reply = %q", got)
	}
	if got := wakeSend(t, path, strings.Repeat("a", wakeSockMaxBytes+1)); got != "error: too large\n" {
		t.Fatalf("oversize reply = %q", got)
	}
	if got := wakeSend(t, path, "письмо"); got != "error: closed now\n" {
		t.Fatalf("enqueue reply = %q", got)
	}
	_, calls, _ := fake.snapshot()
	if calls != 1 {
		t.Fatalf("enqueue calls = %d, want 1", calls)
	}

	slot.set(nil)
	if got := wakeSend(t, path, "ещё"); got != "error: no session\n" {
		t.Fatalf("nil session reply = %q", got)
	}
}

func TestWakeSocketDoesNotStealLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.sock")
	firstFake := &wakeFake{}
	firstSlot := &wakeSlot{}
	firstSlot.set(firstFake)
	first, err := listenWake(path, firstSlot)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	second, err := listenWake(path, &wakeSlot{})
	if second != nil || !errors.Is(err, errWakeBusy) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second listen = %v, %v; want busy", second, err)
	}
	if got := wakeSend(t, path, "первому"); got != "ok\n" {
		t.Fatalf("first listener reply = %q", got)
	}
	_, calls, _ := firstFake.snapshot()
	if calls != 1 {
		t.Fatalf("first listener calls = %d", calls)
	}
}

func TestWakeSocketReplacesStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.sock")
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	fake := &wakeFake{}
	slot := &wakeSlot{}
	slot.set(fake)
	ln, err := listenWake(path, slot)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := wakeSend(t, path, "после старого"); got != "ok\n" {
		t.Fatalf("reply = %q", got)
	}
	ln.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remained after close: %v", err)
	}
}

func TestWakeSocketLeavesOrdinaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wake.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := listenWake(path, &wakeSlot{})
	if ln != nil || !errors.Is(err, errWakeNotSocket) {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("listen = %v, %v", ln, err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "not a socket" {
		t.Fatalf("file changed: %q %v", body, err)
	}
}

func TestWakeSocketSkipsWithoutHome(t *testing.T) {
	t.Setenv("REASONIX_HOME", "")
	ln, err := startWakeSocket(&wakeSlot{})
	if ln != nil || err != nil {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("unset home listen = %v, %v", ln, err)
	}

	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("REASONIX_HOME", missing)
	ln, err = startWakeSocket(&wakeSlot{})
	if ln != nil || !errors.Is(err, errWakeNoHome) {
		if ln != nil {
			ln.Close()
		}
		t.Fatalf("missing home listen = %v, %v", ln, err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("missing home was created: %v", statErr)
	}
}

func TestControllerUserMessageReachesTranscript(t *testing.T) {
	m := newChatTUI(newOwnedTestController(t, control.Options{}), "", make(chan event.Event, 1), 80)
	before := len(m.transcript)
	next, _ := m.Update(agentEventMsg(event.Event{Kind: event.UserMessage, Text: "привет"}))
	got := next.(chatTUI)
	if len(got.transcript) <= before {
		t.Fatal("controller user message did not reach the transcript")
	}
	joined := strings.Join(got.transcript, "\n")
	if !strings.Contains(joined, "привет") || !strings.Contains(joined, "›") {
		t.Fatalf("transcript = %q", joined)
	}
	if !got.bubblePending {
		t.Fatal("controller user message left the bubble untracked")
	}
}

func TestIncomingLetterIsNotOperatorInput(t *testing.T) {
	m := newChatTUI(newOwnedTestController(t, control.Options{}), "", make(chan event.Event, 1), 80)
	display, submit := wakeTurn("Grok: проверь канал")
	if !strings.Contains(submit, "Grok: проверь канал") || strings.Contains(submit, wakeIncomingMark) {
		t.Fatalf("submit = %q", submit)
	}
	next, _ := m.Update(agentEventMsg(event.Event{Kind: event.UserMessage, Text: display}))
	got := next.(chatTUI)
	joined := strings.Join(got.transcript, "\n")
	if !strings.Contains(joined, "← Grok: проверь канал") || strings.Contains(joined, "›") {
		t.Fatalf("transcript = %q", joined)
	}
	if got.bubblePending || got.pendingRestore != "" {
		t.Fatal("incoming letter became the operator draft")
	}
}

func TestComposerTurnIgnoresDuplicateUserMessage(t *testing.T) {
	m := newChatTUI(newOwnedTestController(t, control.Options{}), "", make(chan event.Event, 1), 80)
	m.bubblePending = true
	m.pendingRestore = "typed"
	before := append([]string(nil), m.transcript...)
	next, _ := m.Update(agentEventMsg(event.Event{Kind: event.UserMessage, Text: "typed"}))
	got := next.(chatTUI)
	if strings.Join(got.transcript, "\n") != strings.Join(before, "\n") {
		t.Fatalf("composer turn duplicated the bubble: %q", got.transcript)
	}
	if !got.bubblePending || got.pendingRestore != "typed" {
		t.Fatal("local user event confirmed the composer bubble")
	}
}

func TestConfirmedComposerTurnDoesNotRepeatUserLine(t *testing.T) {
	m := newChatTUI(newOwnedTestController(t, control.Options{}), "", make(chan event.Event, 1), 80)
	m.echoedUser = "Здравстуй Зерде."
	m.bubblePending = false
	m.transcript = []string{"  › Здравстуй Зерде."}
	before := strings.Join(m.transcript, "\n")
	next, _ := m.Update(agentEventMsg(event.Event{Kind: event.UserMessage, Text: "Здравстуй Зерде."}))
	got := next.(chatTUI)
	if strings.Join(got.transcript, "\n") != before {
		t.Fatalf("repeated user line: %q", got.transcript)
	}
}

func TestDifferentFollowUpStillAppears(t *testing.T) {
	m := newChatTUI(newOwnedTestController(t, control.Options{}), "", make(chan event.Event, 1), 80)
	m.echoedUser = "первое"
	m.bubblePending = false
	next, _ := m.Update(agentEventMsg(event.Event{Kind: event.UserMessage, Text: "второе"}))
	got := next.(chatTUI)
	if !strings.Contains(strings.Join(got.transcript, "\n"), "второе") {
		t.Fatalf("follow-up missing: %q", got.transcript)
	}
}

func TestIncomingLetterStillShownAfterEcho(t *testing.T) {
	m := newChatTUI(newOwnedTestController(t, control.Options{}), "", make(chan event.Event, 1), 80)
	m.echoedUser = "Здравстуй Зерде."
	m.bubblePending = false
	display, _ := wakeTurn("Grok: письмо")
	next, _ := m.Update(agentEventMsg(event.Event{Kind: event.UserMessage, Text: display}))
	got := next.(chatTUI)
	joined := strings.Join(got.transcript, "\n")
	if !strings.Contains(joined, "← Grok: письмо") {
		t.Fatalf("incoming missing: %q", joined)
	}
}
