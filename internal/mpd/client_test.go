package mpd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFixtureGreetingAndCommands(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies()})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	if c.Version() != "0.24.0" {
		t.Fatalf("version %q", c.Version())
	}
	status, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := status.Get("state"); !ok || v != "stop" {
		t.Fatalf("state %q ok %v", v, ok)
	}
	if v, ok := status.Get("lastloadedplaylist"); !ok || v != "" {
		t.Fatalf("empty value %q ok %v", v, ok)
	}
	stats, err := c.Stats(context.Background())
	if err != nil || mustGet(t, stats, "songs") != "386" {
		t.Fatalf("stats %v %v", stats, err)
	}
	outs, err := c.Outputs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := ServerOutputs(outs)
	if len(got) != 2 || got[0].Name != "alsa" || !got[0].Enabled || got[0].Plugin != "alsa" {
		t.Fatalf("outputs %#v", got)
	}
	if dop, ok := got[0].Attribute("dop"); !ok || dop != "0" {
		t.Fatalf("dop %q ok %v", dop, ok)
	}
	if got[1].Name != "multiroom" || got[1].Enabled || got[1].Plugin != "fifo" {
		t.Fatalf("second output %#v", got[1])
	}
	gain, err := c.ReplayGainStatus(context.Background())
	if err != nil || mustGet(t, gain, "replay_gain_mode") != "off" {
		t.Fatalf("gain %v %v", gain, err)
	}
	song, err := c.CurrentSong(context.Background())
	if err != nil || len(song.Pairs) != 0 {
		t.Fatalf("currentsong %v %v", song, err)
	}
	cmds, err := c.Commands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := cmds.Get("command"); !ok || v != "add" {
		t.Fatalf("first command %q", v)
	}
}

func TestConfigIsRefusedOverTCP(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies()})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	_, err := c.Config(context.Background())
	var ack *ACKError
	if !errors.As(err, &ack) || ack.Code != ACKPermission || ack.Command != "config" ||
		ack.Message != "Command only permitted to local clients" {
		t.Fatalf("config over TCP: %v", err)
	}
}

func TestWrongPasswordIsAnACKAndDropsTheSecret(t *testing.T) {
	const secret = "wrong-mpd-password-value"
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{
		greeting: fx.Greeting, replies: fx.replies(),
		password: "right-mpd-password-value", echoPassword: true,
	})
	c, err := Dial(context.Background(), srv.addr, secret)
	if c != nil {
		t.Fatal("a failed Dial returned the client, whose read buffer can still hold the echoed password")
	}
	if err == nil {
		t.Fatal("wrong password was accepted")
	}
	var ack *ACKError
	if !errors.As(err, &ack) || ack.Code != ACKPassword {
		t.Fatalf("want a password ACK, got %v", redact(err, secret))
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(ack.Message, secret) || strings.Contains(ack.Command, secret) {
		t.Fatal("the password ACK still carries the password")
	}
	if cmds := srv.commands(); len(cmds) != 1 || cmds[0] != "password" {
		t.Fatalf("server saw %v", cmds)
	}
}

func TestDialDoesNotKeepThePassword(t *testing.T) {
	const secret = "kept-mpd-password-value"
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies(), password: secret})
	c, err := Dial(context.Background(), srv.addr, secret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if holdsSecret(reflect.ValueOf(c), secret, map[uintptr]struct{}{}) {
		t.Fatal("the client still holds the password after Dial")
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if holdsSecret(reflect.ValueOf(c), secret, map[uintptr]struct{}{}) {
		t.Fatal("the client holds the password after a command")
	}
}

func TestDialRefusesAnUnexpandedSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Dial(ctx, "~/socket", "")
	if err == nil || strings.Contains(err.Error(), "dial") {
		t.Fatalf("relative socket was dialled: %v", err)
	}
	_, err = Dial(ctx, "$XDG_RUNTIME_DIR/socket", "")
	if err == nil || strings.Contains(err.Error(), "dial") || strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatalf("unexpanded socket was dialled: %v", err)
	}
}

func TestGreetingAndReplyFramingIsExact(t *testing.T) {
	version, err := ParseGreeting("OK MPD 0.24.0")
	if err != nil || version != "0.24.0" {
		t.Fatalf("greeting %q %v", version, err)
	}
	for _, line := range []string{"  OK MPD 0.24.0", "OK MPD    0.24.0", "OK MPD 0.24.0 "} {
		if _, err := ParseGreeting(line); err == nil {
			t.Fatalf("accepted greeting %q", line)
		}
	}
	if _, err := ParseReply("key: value\nOK\n"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"  OK  \n", "OK \n", "OK MPD 0.24.0\n"} {
		if _, err := ParseReply(text); err == nil {
			t.Fatalf("accepted reply %q", text)
		}
	}
}

func TestBinaryChunkDoesNotEndTheReply(t *testing.T) {
	fx := loadReplies(t)
	// Four payload bytes are "\nOK\n", then the chunk's own newline, then
	// the real pair and the real OK. A line reader that stops on OK ends
	// inside the payload.
	body := "binary: 4\n\nOK\n\nkey: kept\nOK\n"
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: map[string]string{"status": body}})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	got, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Get("key"); !ok || v != "kept" {
		t.Fatalf("status %#v", got)
	}
}

// TestMalformedBinarySizeIsAFramingError pins that a line whose key is
// binary, but whose size is missing, malformed or over the cap, is a
// framing error in both readers. The payload below is itself a reply,
// so a reader that keeps the line as a pair ends on the OK inside it.
func TestMalformedBinarySizeIsAFramingError(t *testing.T) {
	const want = "mpd: binary chunk has a bad size"
	const payload = "key: value\nOK\n"
	lines := []string{
		"binary: -1",
		"binary: 01",
		"binary: 04",
		"binary: abc",
		"binary:",
		"binary: ",
		"binary:4",
		"binary: " + strconv.Itoa(MaxReplyBytes+1),
		"binary: 99999999",
		"binary: 1000000",
	}
	for _, line := range lines {
		text := line + "\n" + payload
		if _, err := ParseReply(text); err == nil || err.Error() != want {
			t.Fatalf("ParseReply %q: %v", line, err)
		}
		if _, err := readReplyText(bufio.NewReader(strings.NewReader(text))); err == nil || err.Error() != want {
			t.Fatalf("readReplyText %q: %v", line, err)
		}
	}
	// N equal to the cap is a size. The chunk is short of N bytes, so
	// the failure is the chunk, not a bad size.
	atCap := "binary: " + strconv.Itoa(MaxReplyBytes) + "\nOK\n"
	if _, err := ParseReply(atCap); err == nil || err.Error() == want {
		t.Fatalf("ParseReply at cap: %v", err)
	}
	if _, err := readReplyText(bufio.NewReader(strings.NewReader(atCap))); err == nil || err.Error() == want {
		t.Fatalf("readReplyText at cap: %v", err)
	}
	// A zero length is a size. The chunk is the newline that follows it.
	if _, err := ParseReply("binary: 0\n\nOK\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := readReplyText(bufio.NewReader(strings.NewReader("binary: 0\n\nOK\n"))); err != nil {
		t.Fatal(err)
	}
}

func TestHoldsSecretWalksContainers(t *testing.T) {
	const secret = "container-secret-value"
	type inner struct{ S string }
	type box struct{ V any }
	var word [1]string
	word[0] = secret
	var raw [8]byte
	copy(raw[:], "secret12")
	seen := map[uintptr]struct{}{}
	for _, v := range []reflect.Value{
		reflect.ValueOf(map[string]string{"k": secret}),
		reflect.ValueOf(word),
		reflect.ValueOf(&inner{S: secret}),
		reflect.ValueOf(box{V: secret}),
	} {
		if !holdsSecret(v, secret, seen) {
			t.Fatalf("%s hid the secret", v.Type())
		}
	}
	if !holdsSecret(reflect.ValueOf(raw), "secret12", seen) {
		t.Fatal("a byte array hid the secret")
	}
	if holdsSecret(reflect.ValueOf(map[string]int{"a": 1}), secret, seen) {
		t.Fatal("a map without the secret was reported")
	}
}

func TestAllowlistIsExactlyTheReadCommands(t *testing.T) {
	want := []string{
		"commands", "config", "currentsong", "outputs",
		"password", "replay_gain_status", "stats", "status",
	}
	if len(allowedCommands) != len(want) {
		t.Fatalf("allowlist has %d commands, want %d", len(allowedCommands), len(want))
	}
	for _, name := range want {
		if _, ok := allowedCommands[name]; !ok {
			t.Fatalf("allowlist missing %s", name)
		}
	}
}

func TestExchangeRefusesMutatingCommands(t *testing.T) {
	const secret = "mutating-command-secret"
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies()})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	for _, name := range []string{
		"play", "pause", "stop", "next", "previous", "seek", "seekcur",
		"add", "delete", "update", "rescan", "kill", "setvol", "volume",
		"clear", "load", "save", "enableoutput", "disableoutput", "idle", "close",
	} {
		srv.clear()
		for _, arg := range []string{"", secret} {
			_, err := c.exchange(context.Background(), name, arg)
			if err == nil || err.Error() != "mpd: command refused" {
				t.Fatalf("%s arg-empty %v: %v", name, arg == "", redact(err, secret))
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: the refusal names the argument", name)
			}
			if got := srv.commands(); len(got) != 0 {
				t.Fatalf("%s was written", name)
			}
		}
	}
}

func TestEveryExportedMethodSendsAnAllowlistedCommand(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies()})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	want := map[string]string{
		"Status": "status", "Stats": "stats", "Outputs": "outputs",
		"ReplayGainStatus": "replay_gain_status", "CurrentSong": "currentsong",
		"Config": "config", "Commands": "commands",
	}
	typ := reflect.TypeOf(c)
	called := 0
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if m.Name == "Close" || m.Name == "Version" {
			continue
		}
		if m.Type.NumIn() != 2 || m.Type.NumOut() != 2 {
			t.Fatalf("exported method %s has shape %s; every read is (context.Context) (Reply, error)", m.Name, m.Type)
		}
		cmd, ok := want[m.Name]
		if !ok {
			t.Fatalf("exported method %s has no allowlisted command", m.Name)
		}
		srv.clear()
		out := m.Func.Call([]reflect.Value{
			reflect.ValueOf(c),
			reflect.ValueOf(context.Background()),
		})
		got := srv.commands()
		if len(got) != 1 || got[0] != cmd {
			t.Fatalf("%s sent %v, want [%s]", m.Name, got, cmd)
		}
		if _, ok := allowedCommands[got[0]]; !ok {
			t.Fatalf("%s sent %q, which is not allowlisted", m.Name, got[0])
		}
		if err, _ := out[1].Interface().(error); err != nil && m.Name != "Config" {
			t.Fatalf("%s: %v", m.Name, err)
		}
		called++
	}
	if called != len(want) {
		t.Fatalf("called %d methods, want %d", called, len(want))
	}
}

func TestSlowServerHitsTheDeadline(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies(), mode: "slow"})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, srv.addr, "")
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("slow greeting: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("slow greeting waited %s", time.Since(start))
	}
}

func TestLongLineHitsTheCap(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, mode: "longline"})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	_, err := c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "length cap") {
		t.Fatalf("long line: %v", err)
	}
}

func TestHugeReplyHitsTheCap(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, mode: "flood"})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	start := time.Now()
	_, err := c.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "size cap") {
		t.Fatalf("flood: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("flood waited %s", time.Since(start))
	}
}

func TestDialUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "mpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	fx := loadReplies(t)
	replies := fx.replies()
	replies["config"] = "music_directory: /from-socket\nOK\n"
	srv := startFake(t, fakeOpts{network: "unix", addr: sock, greeting: fx.Greeting, replies: replies})
	c := mustDial(t, srv.addr, "")
	t.Cleanup(func() { _ = c.Close() })
	cfg, err := c.Config(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mustGet(t, cfg, "music_directory") != "/from-socket" {
		t.Fatalf("config over a socket: %#v", cfg)
	}
}

func mustDial(t *testing.T, addr, password string) *Client {
	t.Helper()
	c, err := Dial(context.Background(), addr, password)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustGet(t *testing.T, r Reply, key string) string {
	t.Helper()
	v, ok := r.Get(key)
	if !ok {
		t.Fatalf("no %s in %#v", key, r.Pairs)
	}
	return v
}

type replyFixture struct {
	Greeting         string `json:"greeting"`
	Status           string `json:"status"`
	Stats            string `json:"stats"`
	Outputs          string `json:"outputs"`
	ReplayGainStatus string `json:"replay_gain_status"`
	Config           string `json:"config"`
	CurrentSong      string `json:"currentsong"`
	Commands         string `json:"commands"`
}

func (f replyFixture) replies() map[string]string {
	return map[string]string{
		"status": f.Status, "stats": f.Stats, "outputs": f.Outputs,
		"replay_gain_status": f.ReplayGainStatus, "config": f.Config,
		"currentsong": f.CurrentSong, "commands": f.Commands,
	}
}

func loadReplies(t *testing.T) replyFixture {
	t.Helper()
	b, err := os.ReadFile("testdata/volumio-mpd-replies.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx replyFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

type fakeOpts struct {
	network      string
	addr         string
	greeting     string
	replies      map[string]string
	password     string
	echoPassword bool
	mode         string
}

type fakeMPD struct {
	addr string
	opt  fakeOpts
	ln   net.Listener
	stop chan struct{}
	mu   sync.Mutex
	cmds []string
}

func startFake(t *testing.T, opt fakeOpts) *fakeMPD {
	t.Helper()
	if opt.network == "" {
		opt.network = "tcp"
	}
	if opt.addr == "" {
		opt.addr = "127.0.0.1:0"
	}
	ln, err := net.Listen(opt.network, opt.addr)
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeMPD{addr: ln.Addr().String(), opt: opt, ln: ln, stop: make(chan struct{})}
	if opt.network == "unix" {
		s.addr = opt.addr
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	t.Cleanup(func() {
		close(s.stop)
		_ = ln.Close()
	})
	return s
}

func (s *fakeMPD) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *fakeMPD) clear() {
	s.mu.Lock()
	s.cmds = nil
	s.mu.Unlock()
}

func (s *fakeMPD) record(name string) {
	s.mu.Lock()
	s.cmds = append(s.cmds, name)
	s.mu.Unlock()
}

func (s *fakeMPD) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	switch s.opt.mode {
	case "slow":
		select {
		case <-time.After(2 * time.Second):
		case <-s.stop:
			return
		}
	case "longline":
		_, _ = conn.Write([]byte(s.opt.greeting))
		_, _ = conn.Write([]byte(strings.Repeat("A", MaxLineBytes+64)))
		<-s.stop
		return
	case "flood":
		_, _ = conn.Write([]byte(s.opt.greeting))
		chunk := []byte("k: v\n")
		for n := 0; n < MaxReplyBytes*2; n += len(chunk) {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
		return
	}
	if _, err := conn.Write([]byte(s.opt.greeting)); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		name, arg := splitCommand(strings.TrimRight(line, "\r\n"))
		s.record(name)
		if name == "password" {
			if arg == s.opt.password {
				_, _ = conn.Write([]byte("OK\n"))
			} else if s.opt.echoPassword {
				_, _ = conn.Write([]byte("ACK [3@0] {password} " + arg + "\n"))
			} else {
				_, _ = conn.Write([]byte("ACK [3@0] {password} incorrect password\n"))
			}
			continue
		}
		body, ok := s.opt.replies[name]
		if !ok {
			_, _ = conn.Write([]byte("ACK [5@0] {} unknown command\n"))
			continue
		}
		if _, err := conn.Write([]byte(body)); err != nil {
			return
		}
	}
}

func splitCommand(line string) (name, arg string) {
	if rest, ok := strings.CutPrefix(line, "password "); ok {
		return "password", unquoteArg(strings.TrimSpace(rest))
	}
	return line, ""
}

func unquoteArg(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s)-1; i++ {
		if s[i] == '\\' && i+1 < len(s)-1 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// holdsSecret walks the value the client keeps. It follows pointers,
// interfaces, structs, slices, arrays and maps, including a [N]byte, so
// a later field of any of those shapes that stores the password fails
// TestDialDoesNotKeepThePassword. The socket itself is not walked:
// Write copies the bytes and does not retain the caller's slice, and
// that copy is the kernel's, not a field of Client. A failed Dial
// returns no Client, so the bufio.Reader that read an ACK is not
// reachable after the call returns.
func holdsSecret(v reflect.Value, secret string, seen map[uintptr]struct{}) bool {
	if !v.IsValid() || secret == "" {
		return false
	}
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() || v.Type() == reflect.TypeOf((*net.Conn)(nil)).Elem() {
			return false
		}
		return holdsSecret(v.Elem(), secret, seen)
	case reflect.Pointer:
		if v.IsNil() {
			return false
		}
		if _, ok := seen[v.Pointer()]; ok {
			return false
		}
		seen[v.Pointer()] = struct{}{}
		return holdsSecret(v.Elem(), secret, seen)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if holdsSecret(v.Field(i), secret, seen) {
				return true
			}
		}
	case reflect.String:
		return strings.Contains(v.String(), secret)
	case reflect.Slice:
		if v.IsNil() {
			return false
		}
		if v.Type().Elem().Kind() == reflect.Uint8 && v.Len() > 0 {
			if strings.Contains(string(v.Bytes()), secret) {
				return true
			}
		}
		for i := 0; i < v.Len(); i++ {
			if holdsSecret(v.Index(i), secret, seen) {
				return true
			}
		}
	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 && v.Len() > 0 {
			buf := make([]byte, v.Len())
			for i := 0; i < v.Len(); i++ {
				buf[i] = byte(v.Index(i).Uint())
			}
			if strings.Contains(string(buf), secret) {
				return true
			}
		}
		for i := 0; i < v.Len(); i++ {
			if holdsSecret(v.Index(i), secret, seen) {
				return true
			}
		}
	case reflect.Map:
		if v.IsNil() {
			return false
		}
		iter := v.MapRange()
		for iter.Next() {
			if holdsSecret(iter.Key(), secret, seen) || holdsSecret(iter.Value(), secret, seen) {
				return true
			}
		}
	}
	return false
}
