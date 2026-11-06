//go:build !windows

package main

import (
	"fmt"
	"image"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func platformInit()           {}
func attachParentConsole()    {}
func showMessage(_, _ string) {}

func osDescription() string {
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(line[len("PRETTY_NAME="):], `"`)
			}
		}
	}
	return runtime.GOOS
}

func hideWindow(_ *exec.Cmd) {}

func shellCommand(shell, command string) (string, []string) {
	sh := shell
	if sh == "" || sh == "powershell" || sh == "pwsh" {
		if _, err := exec.LookPath("bash"); err == nil {
			sh = "bash"
		} else {
			sh = "sh"
		}
	}
	return sh, []string{"-lc", command}
}

func decodeConsole(b []byte) string { return string(b) }

func systemRoots() []Root { return nil }

func openTarget(target string) error {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	return exec.Command(bin, target).Start()
}

func setAutostart(enable bool) error {
	return fmt.Errorf("autostart not implemented on %s", runtime.GOOS)
}
func autostartEnabled() bool { return false }

// desktopToast shows a native notification where a tool is available (notify-send / terminal-notifier).
func desktopToast(title, text string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("terminal-notifier"); err == nil {
			cmd = exec.Command("terminal-notifier", "-title", title, "-message", text)
		} else {
			script := "display notification " + macQuote(text) + " with title " + macQuote(title)
			cmd = exec.Command("osascript", "-e", script)
		}
	} else if _, err := exec.LookPath("notify-send"); err == nil {
		cmd = exec.Command("notify-send", title, text)
	}
	if cmd != nil {
		cmd.Start()
		go func() { cmd.Wait() }()
	}
}

func macQuote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\"" }

func ensureFirewall(port int) {} // handled by the OS firewall UI on macOS/Linux

// ---- screen/input: only meaningful on a desktop with a display server; stubbed for the test host ----

func screenSupported() bool { return runtime.GOOS == "darwin" }

func screenSize() (int, int) { return 0, 0 }

func captureScreen() (*image.RGBA, error) {
	return nil, fmt.Errorf("screen capture not supported on %s", runtime.GOOS)
}

func listMonitors() []Monitor {
	w, h := screenSize()
	return []Monitor{{W: w, H: h, Primary: true, Name: "primary"}}
}

func captureMonitor(int) (*image.RGBA, Monitor, error) {
	img, err := captureScreen()
	return img, Monitor{Primary: true}, err
}

func lockSession() error { return fmt.Errorf("lock not supported on %s", runtime.GOOS) }

func doMouse(MouseInput) error        { return fmt.Errorf("input not supported on %s", runtime.GOOS) }
func doKeyCombo(string, string) error { return fmt.Errorf("input not supported on %s", runtime.GOOS) }
func typeText(string) error           { return fmt.Errorf("input not supported on %s", runtime.GOOS) }

// clipboardSeq: no cheap change counter here; the watcher polls the text instead.
func clipboardSeq() uint32 { return 0 }

func clipboardPrivate() bool { return false }

func winClipHistory() (supported, on bool) { return false, false }

func setWinClipHistory(bool) error { return fmt.Errorf("clipboard history is a Windows feature") }

func clipboardGet() (string, error) {
	for _, c := range [][]string{{"pbpaste"}, {"xclip", "-selection", "clipboard", "-o"}, {"xsel", "-b"}, {"wl-paste"}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			out, err := exec.Command(c[0], c[1:]...).Output()
			return string(out), err
		}
	}
	return "", nil
}

func clipboardSet(text string) error {
	for _, c := range [][]string{{"pbcopy"}, {"xclip", "-selection", "clipboard"}, {"xsel", "-b", "-i"}, {"wl-copy"}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			cmd := exec.Command(c[0], c[1:]...)
			cmd.Stdin = strings.NewReader(text)
			return cmd.Run()
		}
	}
	return nil
}
