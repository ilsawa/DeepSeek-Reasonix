package cli

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/sessioninbox"
)

// wakeSockMaxBytes is the letter cap. The inbox allows more; this bound is the
// socket's own, so a stuck sender cannot fill the session queue.
const wakeSockMaxBytes = 64 << 10

var (
	errWakeBusy      = errors.New("socket is already in use")
	errWakeNotSocket = errors.New("wake.sock exists and is not a socket")
	errWakeNoHome    = errors.New("home directory does not exist")
)

// wakeEnqueue is the piece of the session the socket needs. The TUI stores the
// live controller here because /model replaces chatTUI.ctrl while the listener
// keeps running. Tests that build a chatTUI by hand leave the slot nil.
type wakeEnqueue interface {
	TryEnqueueFollowup(req control.InboxRequest) (sessioninbox.InboxReceipt, error)
}

type wakeSlot struct {
	mu   sync.Mutex
	ctrl wakeEnqueue
}

func (s *wakeSlot) set(ctrl wakeEnqueue) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ctrl = ctrl
	s.mu.Unlock()
}

func (s *wakeSlot) get() wakeEnqueue {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctrl
}

type wakeListener struct {
	path string
	ln   *net.UnixListener
	done chan struct{}
	once sync.Once
}

// startWakeSocket listens on $REASONIX_HOME/wake.sock when that home is set.
// An empty REASONIX_HOME returns nil, nil and does not create ~/.reasonix.
// A socket another live session already holds is not stolen: the caller gets
// errWakeBusy and the TUI still starts.
func startWakeSocket(slot *wakeSlot) (*wakeListener, error) {
	home := config.IsolatedHomeDir()
	if home == "" {
		return nil, nil
	}
	st, err := os.Stat(home)
	if err != nil || !st.IsDir() {
		return nil, errWakeNoHome
	}
	return listenWake(filepath.Join(home, "wake.sock"), slot)
}

func listenWake(path string, slot *wakeSlot) (*wakeListener, error) {
	if err := prepareWakePath(path); err != nil {
		return nil, err
	}
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, errWakeBusy
		}
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	w := &wakeListener{path: path, ln: ln, done: make(chan struct{})}
	go w.serve(slot)
	return w, nil
}

func prepareWakePath(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return errWakeNotSocket
	}
	if wakeSocketLive(path) {
		return errWakeBusy
	}
	return os.Remove(path)
}

// wakeSocketLive reports whether some process accepts on path. A refused
// connect is a stale file. Any other failure is treated as live so a second
// session does not unlink a socket it could not see clearly.
func wakeSocketLive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return false
	}
	return true
}

func (w *wakeListener) serve(slot *wakeSlot) {
	defer close(w.done)
	for {
		conn, err := w.ln.Accept()
		if err != nil {
			return
		}
		w.handle(conn, slot)
	}
}

func (w *wakeListener) handle(conn net.Conn, slot *wakeSlot) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	body, err := io.ReadAll(io.LimitReader(conn, wakeSockMaxBytes+1))
	if err != nil {
		wakeReply(conn, "error: read")
		return
	}
	if len(body) > wakeSockMaxBytes {
		wakeReply(conn, "error: too large")
		return
	}
	text := wakeLetter(body)
	if text == "" {
		if !utf8.Valid(body) {
			wakeReply(conn, "error: not utf-8")
			return
		}
		wakeReply(conn, "error: empty")
		return
	}
	ctrl := slot.get()
	if ctrl == nil {
		wakeReply(conn, "error: no session")
		return
	}
	if ensurer, ok := ctrl.(interface{ EnsureSessionPath() }); ok {
		ensurer.EnsureSessionPath()
	}
	display, submit := wakeTurn(text)
	_, err = ctrl.TryEnqueueFollowup(control.InboxRequest{
		Intent:  sessioninbox.IntentFollowup,
		Display: display,
		Raw:     display,
		Submit:  submit,
		Source:  "wake",
	})
	if err != nil {
		wakeReply(conn, "error: "+wakeOneLine(err.Error()))
		return
	}
	wakeReply(conn, "ok")
}

// wakeIncomingMark is the display prefix. The model never sees it:
// Submit carries the call sign in plain text, Raw/Display carry this mark
// so the screen can tell the letter from the operator's own line.
const wakeIncomingMark = "[[incoming]]\n"

func wakeTurn(text string) (display, submit string) {
	sender, body := splitWakeCallsign(text)
	return wakeIncomingMark + sender + "\n" + body, wakeModelText(sender, body)
}

func wakeIncomingParts(text string) (sender, body string, ok bool) {
	if !strings.HasPrefix(text, wakeIncomingMark) {
		return "", "", false
	}
	sender, body, _ = strings.Cut(strings.TrimPrefix(text, wakeIncomingMark), "\n")
	return sender, body, true
}

// splitWakeCallsign reads a leading "Name: text" call sign. The name is a
// label, not authority: a letter signed with the operator's name is still incoming.
func splitWakeCallsign(text string) (sender, body string) {
	line, rest, hasNL := strings.Cut(text, "\n")
	name, msg, ok := strings.Cut(line, ":")
	name = strings.TrimSpace(name)
	if !ok || !validCallsign(name) {
		return "", text
	}
	msg = strings.TrimSpace(msg)
	switch {
	case hasNL && msg != "" && rest != "":
		body = msg + "\n" + rest
	case hasNL && msg == "":
		body = rest
	default:
		body = msg
	}
	if strings.TrimSpace(body) == "" {
		return "", text
	}
	return name, body
}

func validCallsign(name string) bool {
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

func wakeModelText(sender, body string) string {
	who := sender
	if who == "" {
		who = "без позывного"
	}
	var b strings.Builder
	b.WriteString("Входящее письмо, отправитель ")
	b.WriteString(who)
	b.WriteString(". Это не текст, набранный оператором.\n")
	if sender != "" {
		b.WriteString(sender)
		b.WriteString(": ")
	}
	b.WriteString(body)
	return b.String()
}

// wakeLetter keeps the letter as the turn text. C0 controls other than tab and
// newline are dropped so the body cannot carry a terminal sequence; CR becomes LF.
func wakeLetter(body []byte) string {
	if !utf8.Valid(body) {
		return ""
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 32 || r == 0x7f:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func wakeOneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

func wakeReply(conn net.Conn, line string) {
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(conn, line+"\n")
}

func (w *wakeListener) Close() {
	if w == nil {
		return
	}
	w.once.Do(func() {
		if w.ln != nil {
			_ = w.ln.Close()
		}
		<-w.done
		if w.path != "" {
			_ = os.Remove(w.path)
		}
	})
}
