//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func autoStartFile() string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", "com.phonedrop.app.plist")
	}
	return filepath.Join(home, ".config", "autostart", "phonedrop.desktop")
}

func getAutoStart() bool {
	_, err := os.Stat(autoStartFile())
	return err == nil
}

func setAutoStart(on bool) error {
	file := autoStartFile()
	if !on {
		if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if v := os.Getenv("APPIMAGE"); v != "" {
		exe = v
	}
	var body string
	if runtime.GOOS == "darwin" {
		body = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.phonedrop.app</string>
  <key>ProgramArguments</key><array><string>%s</string><string>--hidden</string></array>
  <key>RunAtLoad</key><true/>
</dict></plist>
`, exe)
	} else {
		body = fmt.Sprintf("[Desktop Entry]\nType=Application\nName=PhoneDrop\nExec=\"%s\" --hidden\nX-GNOME-Autostart-enabled=true\n", exe)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, []byte(body), 0o644)
}
