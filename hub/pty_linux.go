//go:build linux

package main

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// linuxPTY is a real pseudo-terminal built on /dev/ptmx, so interactive shells
// (prompts, colors, line editing) behave exactly as on a console.
type linuxPTY struct {
	ptmx *os.File
	cmd  *exec.Cmd
	once sync.Once
	code int
}

const (
	tiocgptn   = 0x80045430
	tiocsptlck = 0x40045431
	tiocswinsz = 0x5414
)

type winsize struct {
	rows, cols, x, y uint16
}

func startPTY(shell string, cols, rows int) (PTY, error) {
	ptmx, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var unlock int32 = 0
	syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), tiocsptlck, uintptr(unsafe.Pointer(&unlock)))
	var ptn uint32
	syscall.Syscall(syscall.SYS_IOCTL, ptmx.Fd(), tiocgptn, uintptr(unsafe.Pointer(&ptn)))
	slaveName := "/dev/pts/" + itoa(int(ptn))
	slave, err := os.OpenFile(slaveName, os.O_RDWR, 0)
	if err != nil {
		ptmx.Close()
		return nil, err
	}

	name, args := shellCommand(shell, "")
	if len(args) >= 2 {
		args = nil // interactive, no -c
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Ctty is the index into the child's fd table (stdin=0), not the parent fd number.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	if err := cmd.Start(); err != nil {
		ptmx.Close()
		slave.Close()
		return nil, err
	}
	slave.Close() // child holds it now
	p := &linuxPTY{ptmx: ptmx, cmd: cmd}
	p.Resize(cols, rows)
	return p, nil
}

func (p *linuxPTY) Read(b []byte) (int, error)  { return p.ptmx.Read(b) }
func (p *linuxPTY) Write(b []byte) (int, error) { return p.ptmx.Write(b) }

func (p *linuxPTY) Resize(cols, rows int) error {
	ws := winsize{rows: uint16(rows), cols: uint16(cols)}
	syscall.Syscall(syscall.SYS_IOCTL, p.ptmx.Fd(), tiocswinsz, uintptr(unsafe.Pointer(&ws)))
	return nil
}

func (p *linuxPTY) Wait() int {
	err := p.cmd.Wait()
	p.ptmx.Close()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 0
}

func (p *linuxPTY) Close() error {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		p.ptmx.Close()
	})
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
