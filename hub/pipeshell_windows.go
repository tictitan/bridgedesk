//go:build windows

package main

import (
	"io"
	"os/exec"
	"sync"
)

// pipeShell is the fallback terminal used when ConPTY can't be set up. It runs cmd.exe with
// echo off (/Q) and we echo keystrokes ourselves, so typing is visible and commands run even
// without a pseudo-console. No full TUI/colors, but reliable. A banner marks it so it's
// recognizable as the compatibility path. From here you can type `powershell` for a PS session.
type pipeShell struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	pr      *io.PipeReader
	pw      *io.PipeWriter
	once    sync.Once
	wmu     sync.Mutex
	outDone bool
}

func startPipeShell(shell string) (PTY, error) {
	cmd := exec.Command("cmd.exe", "/Q", "/K", "prompt $P$G")
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	p := &pipeShell{cmd: cmd, stdin: stdin, pr: pr, pw: pw}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	copyConv := func(src io.Reader) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, e := src.Read(buf)
			if n > 0 {
				p.emit(crlf(buf[:n]))
			}
			if e != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copyConv(stdout)
	go copyConv(stderr)
	go func() { wg.Wait(); p.pw.Close() }()

	p.emit([]byte("\x1b[90m[BridgeDesk compatibility terminal — ConPTY unavailable on this PC]\x1b[0m\r\n"))
	return p, nil
}

func (p *pipeShell) emit(b []byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.outDone {
		p.pw.Write(b)
	}
}

func crlf(b []byte) []byte {
	out := make([]byte, 0, len(b)+8)
	for i := 0; i < len(b); i++ {
		if b[i] == '\n' && (i == 0 || b[i-1] != '\r') {
			out = append(out, '\r', '\n')
		} else {
			out = append(out, b[i])
		}
	}
	return out
}

func (p *pipeShell) Read(b []byte) (int, error) { return p.pr.Read(b) }

func (p *pipeShell) Write(b []byte) (int, error) {
	// Echo keystrokes locally (cmd /Q won't echo), translating CR to CRLF for xterm + cmd.
	echo := make([]byte, 0, len(b)+8)
	toShell := make([]byte, 0, len(b)+8)
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\r' {
			echo = append(echo, '\r', '\n')
			toShell = append(toShell, '\r', '\n')
		} else if c == 0x7f || c == 0x08 { // backspace: erase one char on screen
			echo = append(echo, '\b', ' ', '\b')
			toShell = append(toShell, 0x08)
		} else {
			echo = append(echo, c)
			toShell = append(toShell, c)
		}
	}
	p.emit(echo)
	if _, err := p.stdin.Write(toShell); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *pipeShell) Resize(cols, rows int) error { return nil }

func (p *pipeShell) Wait() int {
	err := p.cmd.Wait()
	p.wmu.Lock()
	p.outDone = true
	p.wmu.Unlock()
	p.pr.Close()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 0
}

func (p *pipeShell) Close() error {
	p.once.Do(func() {
		p.stdin.Close()
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		p.wmu.Lock()
		p.outDone = true
		p.wmu.Unlock()
		p.pr.Close()
	})
	return nil
}
