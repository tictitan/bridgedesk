package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Shared-storage operations: create/remove an SMB share, and mount/unmount an SMB or NFS path.
// These are thin wrappers over the OS tools (New-SmbShare / net use / mount), so devices across
// the fleet can have real shared drives. Most operations need administrator/root rights.

type ShareInfo struct {
	Name string `json:"name"`
	Path string `json:"path"`
	UNC  string `json:"unc,omitempty"`
}

func shareExec(ctx context.Context, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, name, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return decodeConsole(out), err
}

func firstIP() string {
	if ip := primaryOutboundIP(); ip != "" {
		return ip
	}
	if ips := localIPs(); len(ips) > 0 {
		return ips[0]
	}
	host, _ := os.Hostname()
	return host
}

// shareCreate exposes a folder as an SMB share (Windows). Returns the UNC path to mount elsewhere.
func shareCreate(ctx context.Context, name, path string, readonly bool) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" || path == "" {
		return nil, fmt.Errorf("share name and folder path are required")
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("folder not found: %s", path)
	}
	switch runtime.GOOS {
	case "windows":
		access := "-FullAccess Everyone"
		if readonly {
			access = "-ReadAccess Everyone"
		}
		ps := fmt.Sprintf("New-SmbShare -Name '%s' -Path '%s' %s -ErrorAction Stop | Out-Null; 'ok'",
			psEscape(name), psEscape(path), access)
		out, err := shareExec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
		if err != nil {
			if strings.Contains(out, "Access is denied") || strings.Contains(strings.ToLower(out), "administrator") {
				return nil, fmt.Errorf("creating an SMB share needs administrator — restart BridgeDesk as administrator (tray menu), then retry")
			}
			if strings.Contains(out, "already exists") {
				return nil, fmt.Errorf("a share named %q already exists", name)
			}
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
		unc := fmt.Sprintf(`\\%s\%s`, firstIP(), name)
		return map[string]any{"name": name, "path": path, "unc": unc,
			"mountHint": fmt.Sprintf(`On another PC: net use Z: %s   (or map it in Explorer)`, unc)}, nil
	case "linux", "darwin":
		return nil, fmt.Errorf("creating an SMB share on %s needs Samba configured separately; use the built-in WebDAV drive instead (Settings shows its URL), or mount a share created on a Windows PC", runtime.GOOS)
	}
	return nil, fmt.Errorf("unsupported OS")
}

func shareRemove(ctx context.Context, name string) (map[string]any, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("remove-share is implemented on Windows only")
	}
	ps := fmt.Sprintf("Remove-SmbShare -Name '%s' -Force -ErrorAction Stop | Out-Null; 'ok'", psEscape(name))
	out, err := shareExec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
	if err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return map[string]any{"removed": name}, nil
}

func shareList(ctx context.Context) ([]ShareInfo, error) {
	switch runtime.GOOS {
	case "windows":
		ps := "Get-SmbShare | Where-Object { $_.Name -notlike '*$' } | ForEach-Object { $_.Name + '|' + $_.Path }"
		out, _ := shareExec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
		var list []ShareInfo
		ip := firstIP()
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.Contains(line, "|") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			list = append(list, ShareInfo{Name: parts[0], Path: parts[1], UNC: fmt.Sprintf(`\\%s\%s`, ip, parts[0])})
		}
		return list, nil
	default:
		return []ShareInfo{}, nil
	}
}

// shareMount mounts an SMB (\\server\share or //server/share) or NFS (server:/export) path.
// mountAs is a drive letter (Windows, e.g. "Z:") or a directory (Linux/macOS).
func shareMount(ctx context.Context, source, mountAs, user, pass string) (map[string]any, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, fmt.Errorf("a share path is required")
	}
	auto := mountAs == "" || strings.EqualFold(mountAs, "auto")
	isNFS := nfsRe.MatchString(source) && !strings.HasPrefix(source, `\\`) && !strings.HasPrefix(source, "//")
	switch runtime.GOOS {
	case "windows":
		unc := strings.ReplaceAll(source, "/", `\`)
		if isNFS {
			tgt := mountAs
			if auto {
				tgt = "*"
			}
			out, err := shareExec(ctx, "cmd.exe", "/c", "mount", "-o", "anon", source, tgt)
			if err != nil {
				return nil, fmt.Errorf("NFS mount failed (is the “Client for NFS” Windows feature installed?): %s", strings.TrimSpace(out))
			}
			return map[string]any{"mounted": firstDriveLetter(out, tgt), "source": source}, nil
		}
		// net use {Z:|*} \\server\share [password] [/user:user] /persistent:yes
		tgt := mountAs
		if auto {
			tgt = "*" // let Windows pick the next free drive letter
		}
		args := []string{"use", tgt, unc}
		if pass != "" {
			args = append(args, pass)
		}
		if user != "" {
			args = append(args, "/user:"+user)
		}
		args = append(args, "/persistent:yes")
		out, err := shareExec(ctx, "net", args...)
		if err != nil {
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
		return map[string]any{"mounted": firstDriveLetter(out, tgt), "source": unc}, nil
	case "linux":
		if auto {
			home, _ := os.UserHomeDir()
			mountAs = fmt.Sprintf("%s/BridgeDesk/%s", home, shareBaseName(source))
		}
		os.MkdirAll(mountAs, 0o755)
		var out string
		var err error
		if isNFS {
			out, err = shareExec(ctx, "mount", "-t", "nfs", source, mountAs)
		} else {
			src := strings.ReplaceAll(source, `\`, "/")
			opts := "guest"
			if user != "" {
				opts = "username=" + user + ",password=" + pass
			}
			out, err = shareExec(ctx, "mount", "-t", "cifs", src, mountAs, "-o", opts)
		}
		if err != nil {
			return nil, fmt.Errorf("mount failed (needs root; cifs-utils/nfs-common installed?): %s", strings.TrimSpace(out))
		}
		return map[string]any{"mounted": mountAs, "source": source}, nil
	case "darwin":
		if auto {
			home, _ := os.UserHomeDir()
			mountAs = fmt.Sprintf("%s/BridgeDesk/%s", home, shareBaseName(source))
		}
		os.MkdirAll(mountAs, 0o755)
		if isNFS {
			out, err := shareExec(ctx, "mount", "-t", "nfs", source, mountAs)
			if err != nil {
				return nil, fmt.Errorf("%s", strings.TrimSpace(out))
			}
			return map[string]any{"mounted": mountAs, "source": source}, nil
		}
		src := strings.ReplaceAll(source, `\`, "/")
		src = strings.TrimPrefix(src, "//")
		cred := ""
		if user != "" {
			cred = user + ":" + pass + "@"
		}
		out, err := shareExec(ctx, "mount_smbfs", "//"+cred+src, mountAs)
		if err != nil {
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
		return map[string]any{"mounted": mountAs, "source": source}, nil
	}
	return nil, fmt.Errorf("unsupported OS")
}

func shareUnmount(ctx context.Context, mountAs string) (map[string]any, error) {
	switch runtime.GOOS {
	case "windows":
		out, err := shareExec(ctx, "net", "use", mountAs, "/delete", "/y")
		if err != nil {
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
	default:
		out, err := shareExec(ctx, "umount", mountAs)
		if err != nil {
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
	}
	return map[string]any{"unmounted": mountAs}, nil
}

var nfsRe = regexp.MustCompile(`^[^\\/]+:/`)
var driveRe = regexp.MustCompile(`(?i)([A-Z]):`)

func psEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }

// firstDriveLetter extracts the assigned drive from `net use *` output ("Drive Z: is now connected").
func firstDriveLetter(out, fallback string) string {
	if m := driveRe.FindString(out); m != "" {
		return strings.ToUpper(m)
	}
	if fallback == "*" {
		return "(auto)"
	}
	return fallback
}

func shareBaseName(source string) string {
	s := strings.TrimRight(strings.ReplaceAll(source, `\`, "/"), "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		s = "share"
	}
	return s
}
