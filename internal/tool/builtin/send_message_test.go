package builtin

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSendMessageRejectsUnknownAndEmpty(t *testing.T) {
	tool := sendMessage{}
	got, err := tool.Execute(context.Background(), []byte(`{"to":"claude","text":"готово"}`))
	if err != nil || got != "некому принять: claude" {
		t.Fatalf("unknown = %q %v", got, err)
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"to":"grok","text":"  "}`)); err == nil {
		t.Fatal("empty text was accepted")
	}
	if _, err := tool.Execute(context.Background(), []byte(`{"to":"chat","text":"Зерде:"}`)); err == nil {
		t.Fatal("call sign without a body was accepted")
	}
	got, err = tool.Execute(context.Background(), []byte(`{"to":"Зерде","text":"себе"}`))
	if err != nil || got != "некому принять: Зерде" {
		t.Fatalf("self = %q %v", got, err)
	}
}

func TestSendMessageDirectAndChatAreSeparate(t *testing.T) {
	dir := t.TempDir()
	grokPath := filepath.Join(dir, "incoming.sock")
	chatPath := filepath.Join(dir, "agent.sock")
	otherPath := filepath.Join(dir, "other.sock")
	selfPath := filepath.Join(dir, "self.sock")
	restoreGrok, restoreChat, restoreHomes := grokSocketPath, chatSocketPath, directHomes
	grokSocketPath = func() (string, error) { return grokPath, nil }
	chatSocketPath = func() (string, error) { return chatPath, nil }
	directHomes = func() []directHome {
		return []directHome{
			{sock: otherPath, callsign: "Искин"},
			{sock: selfPath, callsign: "Зерде"},
			{sock: filepath.Join(dir, "again.sock"), callsign: "Искин-2"},
		}
	}
	t.Cleanup(func() {
		grokSocketPath = restoreGrok
		chatSocketPath = restoreChat
		directHomes = restoreHomes
	})

	grok := listenLetters(t, grokPath)
	chat := listenLetters(t, chatPath)
	other := listenLetters(t, otherPath)
	self := listenLetters(t, selfPath)

	out, err := sendMessage{}.Execute(context.Background(), []byte(`{"to":"grok","text":"Зерде: первая\nвторая"}`))
	if err != nil || out != sendWentGrok {
		t.Fatalf("grok = %q %v", out, err)
	}
	if got := grok.take(); got != "Зерде: первая\nвторая" {
		t.Fatalf("grok letter = %q", got)
	}
	if got := chat.takeNone() + other.takeNone() + self.takeNone(); got != "" {
		t.Fatalf("direct grok leaked: %q", got)
	}

	out, err = sendMessage{}.Execute(context.Background(), []byte(`{"to":"chat","text":"в ленту"}`))
	if err != nil || out != sendWentChat {
		t.Fatalf("chat = %q %v", out, err)
	}
	if got := chat.take(); got != "Зерде: в ленту" {
		t.Fatalf("chat letter = %q", got)
	}
	if got := grok.takeNone() + other.takeNone(); got != "" {
		t.Fatalf("chat leaked: %q", got)
	}

	out, err = sendMessage{}.Execute(context.Background(), []byte(`{"to":"Искин","text":"напрямую"}`))
	if err != nil || out != "ушло Искин" {
		t.Fatalf("direct = %q %v", out, err)
	}
	if got := other.take(); got != "Зерде: напрямую" {
		t.Fatalf("other letter = %q", got)
	}
	if got := grok.takeNone() + chat.takeNone() + self.takeNone(); got != "" {
		t.Fatalf("direct other leaked: %q", got)
	}
}

func TestSendMessageNobodyListening(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.sock")
	restore := grokSocketPath
	grokSocketPath = func() (string, error) { return path, nil }
	t.Cleanup(func() { grokSocketPath = restore })

	out, err := sendMessage{}.Execute(context.Background(), []byte(`{"to":"grok","text":"готово"}`))
	if err != nil || out != sendNobody {
		t.Fatalf("missing = %q %v", out, err)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	out, err = sendMessage{}.Execute(context.Background(), []byte(`{"to":"grok","text":"готово"}`))
	if err != nil || out != sendNobody {
		t.Fatalf("refused = %q %v", out, err)
	}
	if strings.Contains(out, "готово") {
		t.Fatal("receipt echoed the letter")
	}
}

func TestSendMessageRefusesAmbiguousHome(t *testing.T) {
	dir := t.TempDir()
	first := listenLetters(t, filepath.Join(dir, "a.sock"))
	second := listenLetters(t, filepath.Join(dir, "b.sock"))
	restore := directHomes
	directHomes = func() []directHome {
		return []directHome{
			{sock: first.path, callsign: "Искин"},
			{sock: second.path, callsign: "Искин"},
		}
	}
	t.Cleanup(func() { directHomes = restore })

	out, err := sendMessage{}.Execute(context.Background(), []byte(`{"to":"Искин","text":"кому"}`))
	if err != nil || out != "некому принять: Искин" {
		t.Fatalf("ambiguous = %q %v", out, err)
	}
	if got := first.takeNone() + second.takeNone(); got != "" {
		t.Fatalf("ambiguous dialed %q", got)
	}
}

type letterBox struct {
	path string
	ch   chan string
}

func listenLetters(t *testing.T, path string) *letterBox {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	box := &letterBox{path: path, ch: make(chan string, 4)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			body, _ := io.ReadAll(io.LimitReader(conn, 1<<16))
			box.ch <- string(body)
			_, _ = io.WriteString(conn, "ok\n")
			conn.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return box
}

func (b *letterBox) take() string {
	select {
	case got := <-b.ch:
		return got
	case <-time.After(2 * time.Second):
		return ""
	}
}

func (b *letterBox) takeNone() string {
	select {
	case got := <-b.ch:
		return got
	default:
		return ""
	}
}
