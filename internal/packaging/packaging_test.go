package packaging

import (
	"strings"
	"testing"
)

func TestRenderLaunchdTemplate(t *testing.T) {
	body, err := render("launchd.plist.tmpl", Params{
		Label:      ServiceLabel,
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/Users/me/Library/Application Support/1-bit-bridge/bridge.yaml",
		WorkingDir: "/Users/me/Library/Application Support/1-bit-bridge/data",
		LogPath:    "/Users/me/Library/Logs/1-bit-bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"<plist version=\"1.0\">",
		ServiceLabel,
		"<string>serve</string>",
		"--config",
		"/Users/me/Library/Application Support/1-bit-bridge/bridge.yaml",
		"KeepAlive",
		"RunAtLoad",
		"SuccessfulExit",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered launchd missing %q\n--\n%s", want, s)
		}
	}
}

func TestRenderSystemdTemplate(t *testing.T) {
	body, err := render("systemd.service.tmpl", Params{
		BinaryPath: "/usr/local/bin/bridge",
		ConfigPath: "/home/me/.config/1-bit-bridge/bridge.yaml",
		WorkingDir: "/home/me/.config/1-bit-bridge/data",
		LogPath:    "/home/me/.local/state/1-bit-bridge/bridge.log",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"Restart=always",
		"ExecStart=/usr/local/bin/bridge serve --config /home/me/.config/1-bit-bridge/bridge.yaml",
		"WantedBy=default.target",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered systemd missing %q\n--\n%s", want, s)
		}
	}
}

func TestDefaultConfigDirNotEmpty(t *testing.T) {
	d, err := DefaultConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if d == "" {
		t.Error("DefaultConfigDir empty")
	}
	if !strings.Contains(d, "1-bit-bridge") {
		t.Errorf("DefaultConfigDir = %q, missing project name", d)
	}
}

func TestDefaultLogPathNotEmpty(t *testing.T) {
	p, err := DefaultLogPath()
	if err != nil {
		t.Fatal(err)
	}
	if p == "" || !strings.Contains(p, "1-bit-bridge") {
		t.Errorf("DefaultLogPath = %q", p)
	}
}
