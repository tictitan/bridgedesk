package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// LocalDevice exposes this machine's capabilities through the same RPC surface used by linked devices.
type LocalDevice struct {
	cfg  *Config
	id   string
	emit func(Event)

	tmu   sync.Mutex
	terms map[string]*termSession
}

type termSession struct {
	pty    PTY
	closed bool
}

// PTY is implemented by ConPTY on Windows and by a pipe-backed shell elsewhere.
type PTY interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() int
}

func NewLocalDevice(cfg *Config) *LocalDevice {
	idPath := filepath.Join(configDir(), "device-id")
	id, _ := os.ReadFile(idPath)
	if len(id) == 0 {
		b := make([]byte, 6)
		rand.Read(b)
		id = []byte(runtime.GOOS[:3] + "-" + hex.EncodeToString(b))
		os.WriteFile(idPath, id, 0o600)
	}
	return &LocalDevice{cfg: cfg, id: strings.TrimSpace(string(id)), terms: map[string]*termSession{}}
}

func (d *LocalDevice) Info() DeviceInfo {
	caps := []string{}
	if d.cfg.AllowShell {
		caps = append(caps, "shell", "term")
	}
	if d.cfg.AllowFiles {
		caps = append(caps, "files")
	}
	if d.cfg.AllowScreen && screenSupported() {
		caps = append(caps, "screen", "input")
	}
	caps = append(caps, "clipboard")
	home, _ := os.UserHomeDir()
	return DeviceInfo{
		ID: d.id, Name: d.cfg.Name, Kind: runtime.GOOS, OS: osDescription(), Caps: caps,
		Online: true, Local: true, HomeDir: home, Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
}

func (d *LocalDevice) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	res, err := d.call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func want(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return nil
	}
	return json.Unmarshal(params, v)
}

func (d *LocalDevice) need(cap string) error {
	ok := map[string]bool{"shell": d.cfg.AllowShell, "files": d.cfg.AllowFiles, "screen": d.cfg.AllowScreen}[cap]
	if !ok {
		return fmt.Errorf("%s access is disabled on %s (enable it in BridgeDesk settings)", cap, d.cfg.Name)
	}
	return nil
}

func (d *LocalDevice) call(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "info":
		return d.Info(), nil

	// ---------- shell ----------
	case "exec":
		if err := d.need("shell"); err != nil {
			return nil, err
		}
		var p struct {
			Command string `json:"command"`
			Shell   string `json:"shell"`
			Cwd     string `json:"cwd"`
			Timeout int    `json:"timeout"`
			Stdin   string `json:"stdin"`
		}
		if err := want(params, &p); err != nil {
			return nil, err
		}
		return runCommand(ctx, p.Command, p.Shell, p.Cwd, p.Stdin, p.Timeout)

	case "term.open":
		if err := d.need("shell"); err != nil {
			return nil, err
		}
		var p struct {
			Cols, Rows int
			Shell      string
		}
		want(params, &p)
		if p.Cols <= 0 {
			p.Cols, p.Rows = 120, 32
		}
		pty, err := startPTY(p.Shell, p.Cols, p.Rows)
		if err != nil {
			return nil, err
		}
		sid := randHex(8)
		s := &termSession{pty: pty}
		d.tmu.Lock()
		d.terms[sid] = s
		d.tmu.Unlock()
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := pty.Read(buf)
				if n > 0 && d.emit != nil {
					p, _ := json.Marshal(map[string]any{"sid": sid, "data": base64.StdEncoding.EncodeToString(buf[:n])})
					d.emit(Event{Event: "term.data", Params: p})
				}
				if err != nil {
					break
				}
			}
			code := pty.Wait()
			if d.emit != nil {
				p, _ := json.Marshal(map[string]any{"sid": sid, "code": code})
				d.emit(Event{Event: "term.exit", Params: p})
			}
			d.tmu.Lock()
			delete(d.terms, sid)
			d.tmu.Unlock()
		}()
		return map[string]string{"sid": sid}, nil

	case "term.write", "term.resize", "term.close":
		var p struct {
			Sid        string `json:"sid"`
			Data       string `json:"data"`
			Cols, Rows int
		}
		want(params, &p)
		d.tmu.Lock()
		s := d.terms[p.Sid]
		d.tmu.Unlock()
		if s == nil {
			return nil, errors.New("terminal session closed")
		}
		switch method {
		case "term.write":
			b, _ := base64.StdEncoding.DecodeString(p.Data)
			_, err := s.pty.Write(b)
			return true, err
		case "term.resize":
			return true, s.pty.Resize(p.Cols, p.Rows)
		default:
			return true, s.pty.Close()
		}

	// ---------- files ----------
	case "fs.roots":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		return fsRoots(d.cfg), nil
	case "fs.list":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ Path string }
		want(params, &p)
		return fsList(p.Path)
	case "fs.stat":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ Path string }
		want(params, &p)
		st, err := os.Stat(p.Path)
		if err != nil {
			return nil, err
		}
		return fileEntry(p.Path, st), nil
	case "fs.read":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct {
			Path   string
			Offset int64
			Length int
		}
		want(params, &p)
		if p.Length <= 0 || p.Length > 4<<20 {
			p.Length = 1 << 20
		}
		f, err := os.Open(p.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		st, _ := f.Stat()
		buf := make([]byte, p.Length)
		n, err := f.ReadAt(buf, p.Offset)
		if err != nil && err != io.EOF {
			return nil, err
		}
		return map[string]any{"data": base64.StdEncoding.EncodeToString(buf[:n]), "eof": p.Offset+int64(n) >= st.Size(), "size": st.Size()}, nil
	case "fs.write":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct {
			Path     string
			Offset   int64
			Data     string
			Truncate bool
		}
		want(params, &p)
		b, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return nil, err
		}
		os.MkdirAll(filepath.Dir(p.Path), 0o755)
		flags := os.O_WRONLY | os.O_CREATE
		if p.Truncate {
			flags |= os.O_TRUNC
		}
		f, err := os.OpenFile(p.Path, flags, 0o644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err := f.WriteAt(b, p.Offset); err != nil {
			return nil, err
		}
		return map[string]any{"written": len(b)}, nil
	case "fs.mkdir":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ Path string }
		want(params, &p)
		return true, os.MkdirAll(p.Path, 0o755)
	case "fs.delete":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct {
			Path      string
			Recursive bool
		}
		want(params, &p)
		if p.Recursive {
			return true, os.RemoveAll(p.Path)
		}
		return true, os.Remove(p.Path)
	case "fs.rename":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ From, To string }
		want(params, &p)
		return true, os.Rename(p.From, p.To)

	// ---------- screen & input ----------
	case "screen.info":
		if err := d.need("screen"); err != nil {
			return nil, err
		}
		w, h := screenSize()
		return map[string]int{"width": w, "height": h}, nil
	case "screen.capture":
		if err := d.need("screen"); err != nil {
			return nil, err
		}
		var p struct {
			Quality  int `json:"quality"`
			MaxWidth int `json:"maxWidth"`
		}
		want(params, &p)
		jpg, w, h, sw, sh, err := captureJPEG(p.Quality, p.MaxWidth)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": base64.StdEncoding.EncodeToString(jpg), "width": sw, "height": sh, "w": w, "h": h, "mime": "image/jpeg"}, nil
	case "input.mouse":
		if err := d.need("screen"); err != nil {
			return nil, err
		}
		var p MouseInput
		want(params, &p)
		return true, doMouse(p)
	case "input.key":
		if err := d.need("screen"); err != nil {
			return nil, err
		}
		var p struct{ Key, Action string }
		want(params, &p)
		return true, doKeyCombo(p.Key, p.Action)
	case "input.text":
		if err := d.need("screen"); err != nil {
			return nil, err
		}
		var p struct{ Text string }
		want(params, &p)
		return true, typeText(p.Text)

	// ---------- clipboard ----------
	case "clipboard.get":
		t, err := clipboardGet()
		return map[string]string{"text": t}, err
	case "clipboard.set":
		var p struct{ Text string }
		want(params, &p)
		return true, clipboardSet(p.Text)

	case "sys.open":
		var p struct{ Target string }
		want(params, &p)
		return true, openTarget(p.Target)

	// ---------- shared storage (SMB / NFS) ----------
	case "share.list":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		return shareList(ctx, d.cfg.ShareUser, d.cfg.SharePass)
	case "share.create":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct {
			Name, Path string
			Readonly   bool
		}
		want(params, &p)
		return shareCreate(ctx, p.Name, p.Path, p.Readonly, d.cfg.ShareUser, d.cfg.SharePass)
	case "share.remove":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ Name string }
		want(params, &p)
		return shareRemove(ctx, p.Name)
	case "share.mount":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ Source, MountAs, User, Pass string }
		want(params, &p)
		return shareMount(ctx, p.Source, p.MountAs, p.User, p.Pass)
	case "share.unmount":
		if err := d.need("files"); err != nil {
			return nil, err
		}
		var p struct{ MountAs string }
		want(params, &p)
		return shareUnmount(ctx, p.MountAs)
	}
	return nil, fmt.Errorf("unknown method %q", method)
}

type MouseInput struct {
	X      float64 `json:"x"` // 0..1 normalized across the screen
	Y      float64 `json:"y"`
	Action string  `json:"action"` // move | down | up | click | dblclick | scroll
	Button string  `json:"button"` // left | right | middle
	DeltaX int     `json:"dx"`
	DeltaY int     `json:"dy"`
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type capBuf struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (c *capBuf) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		room := c.max - c.Len()
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		c.truncated = true
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

func runCommand(ctx context.Context, command, shell, cwd, stdin string, timeoutSec int) (*ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("empty command")
	}
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	if timeoutSec > 10800 { // allow up to 3h for Unreal builds/cooks
		timeoutSec = 10800
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	name, args := shellCommand(shell, command)
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	if cwd != "" {
		cmd.Dir = cwd
	} else if h, err := os.UserHomeDir(); err == nil {
		cmd.Dir = h
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out := &capBuf{max: 2 << 20}
	errb := &capBuf{max: 1 << 20}
	cmd.Stdout, cmd.Stderr = out, errb
	start := time.Now()
	err := cmd.Run()
	res := &ExecResult{Stdout: decodeConsole(out.Bytes()), Stderr: decodeConsole(errb.Bytes()), DurationMs: time.Since(start).Milliseconds(), Truncated: out.truncated || errb.truncated}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return nil, err
		}
	}
	return res, nil
}

type FileEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

func fileEntry(p string, st os.FileInfo) FileEntry {
	return FileEntry{Name: st.Name(), Path: p, Dir: st.IsDir(), Size: st.Size(), MTime: st.ModTime().Unix()}
}

func fsList(dir string) ([]FileEntry, error) {
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(ents))
	for _, e := range ents {
		st, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fe := fileEntry(p, st)
		if st.Mode()&os.ModeSymlink != 0 {
			if t, err := os.Stat(p); err == nil {
				fe.Dir = t.IsDir()
				fe.Size = t.Size()
			}
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

type Root struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func fsRoots(cfg *Config) []Root {
	home, _ := os.UserHomeDir()
	roots := []Root{{"Home", home}}
	for _, sub := range []string{"Desktop", "Documents", "Downloads", "Pictures"} {
		p := filepath.Join(home, sub)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			roots = append(roots, Root{sub, p})
		}
	}
	if cfg.InboxDir != "" {
		os.MkdirAll(cfg.InboxDir, 0o755)
		roots = append(roots, Root{"Inbox (from phone)", cfg.InboxDir})
	}
	roots = append(roots, systemRoots()...)
	return roots
}

func captureJPEG(quality, maxWidth int) (jpg []byte, w, h, sw, sh int, err error) {
	img, err := captureScreen()
	if err != nil {
		return nil, 0, 0, 0, 0, err
	}
	b := img.Bounds()
	sw, sh = b.Dx(), b.Dy()
	var out image.Image = img
	if maxWidth > 0 && sw > maxWidth {
		out = downscale(img, maxWidth)
	}
	if quality <= 0 || quality > 95 {
		quality = 60
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: quality}); err != nil {
		return nil, 0, 0, 0, 0, err
	}
	ob := out.Bounds()
	return buf.Bytes(), ob.Dx(), ob.Dy(), sw, sh, nil
}

// downscale does a fast box-filter-ish nearest sampling (good enough for live streaming).
func downscale(src *image.RGBA, maxW int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dw := maxW
	dh := sh * dw / sw
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		sy := y * sh / dh
		srow := src.Pix[sy*src.Stride:]
		drow := dst.Pix[y*dst.Stride:]
		for x := 0; x < dw; x++ {
			sx := x * sw / dw * 4
			copy(drow[x*4:x*4+4], srow[sx:sx+4])
		}
	}
	return dst
}
