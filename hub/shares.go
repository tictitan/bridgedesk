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
	User string `json:"user,omitempty"` // dedicated account for seamless mounting
	Pass string `json:"pass,omitempty"`
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

// shareCreate exposes a folder as an SMB share (Windows). It provisions a dedicated local account
// and grants it to the share, so other fleet machines mount it seamlessly (no credential prompt).
func shareCreate(ctx context.Context, name, path string, readonly bool, user, pass string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" || path == "" {
		return nil, fmt.Errorf("share name and folder path are required")
	}
	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("folder not found: %s", path)
	}
	switch runtime.GOOS {
	case "windows":
		perm := "M" // modify
		shareAccess := "-FullAccess"
		if readonly {
			perm = "R"
			shareAccess = "-ReadAccess"
		}
		// 1) ensure the dedicated local account exists (hidden from the login screen),
		// 2) grant it NTFS access to the folder, 3) create the share granting that account.
		ps := fmt.Sprintf(`$ErrorActionPreference='Stop'
$u='%s'; $p=ConvertTo-SecureString '%s' -AsPlainText -Force
if (Get-LocalUser -Name $u -ErrorAction SilentlyContinue) { Set-LocalUser -Name $u -Password $p }
else { New-LocalUser -Name $u -Password $p -PasswordNeverExpires -AccountNeverExpires -UserMayNotChangePassword -Description 'BridgeDesk share account' | Out-Null }
# keep the account off the sign-in screen
reg add "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList" /v $u /t REG_DWORD /d 0 /f | Out-Null
icacls "%s" /grant ("${u}:(OI)(CI)%s") | Out-Null
if (Get-SmbShare -Name '%s' -ErrorAction SilentlyContinue) { Remove-SmbShare -Name '%s' -Force | Out-Null }
New-SmbShare -Name '%s' -Path '%s' %s $u -ErrorAction Stop | Out-Null
'ok'`,
			psEscape(user), psEscape(pass), psEscape(path), perm,
			psEscape(name), psEscape(name), psEscape(name), psEscape(path), shareAccess)
		out, err := shareExec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
		if err != nil {
			low := strings.ToLower(out)
			if strings.Contains(low, "access is denied") || strings.Contains(low, "administrator") || strings.Contains(low, "denied") {
				return nil, fmt.Errorf("creating an SMB share needs administrator — use the tray menu → Restart as administrator, then retry")
			}
			return nil, fmt.Errorf("%s", strings.TrimSpace(out))
		}
		unc := fmt.Sprintf(`\\%s\%s`, firstIP(), name)
		return map[string]any{"name": name, "path": path, "unc": unc, "user": user,
			"mountHint": "Other fleet machines can now mount this in one click — no password needed."}, nil
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

// shareList returns the SMB shares this machine offers. user/pass are the dedicated-account
// credentials attached so fleet machines can auto-mount without a prompt.
func shareList(ctx context.Context, user, pass string) ([]ShareInfo, error) {
	switch runtime.GOOS {
	case "windows":
		// Only list shares granted to our dedicated account (the ones we can hand out credentials for).
		ps := fmt.Sprintf("Get-SmbShare | Where-Object { $_.Name -notlike '*$' } | ForEach-Object { $n=$_.Name; $ok=@(Get-SmbShareAccess -Name $n | Where-Object { $_.AccountName -like '*\\%s' -or $_.AccountName -eq '%s' }).Count -gt 0; $n + '|' + $_.Path + '|' + $ok }", psEscape(user), psEscape(user))
		out, _ := shareExec(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
		var list []ShareInfo
		ip := firstIP()
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || !strings.Contains(line, "|") {
				continue
			}
			parts := strings.SplitN(line, "|", 3)
			si := ShareInfo{Name: parts[0], Path: parts[1], UNC: fmt.Sprintf(`\\%s\%s`, ip, parts[0])}
			if len(parts) == 3 && strings.EqualFold(strings.TrimSpace(parts[2]), "true") {
				si.User = user
				si.Pass = pass
			}
			list = append(list, si)
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
		// net use {Z:|*} \\server\share [password] [/user:server\user] /persistent:yes
		tgt := mountAs
		if auto {
			tgt = "*" // let Windows pick the next free drive letter
		}
		args := []string{"use", tgt, unc}
		if pass != "" {
			args = append(args, pass)
		}
		if user != "" {
			// a local account on the server must be qualified with the server's host/IP
			if !strings.ContainsAny(user, `\@`) {
				if host := uncHost(unc); host != "" {
					user = host + `\` + user
				}
			}
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

// uncHost extracts the server host from \\host\share.
func uncHost(unc string) string {
	s := strings.TrimPrefix(strings.ReplaceAll(unc, "/", `\`), `\\`)
	if i := strings.Index(s, `\`); i >= 0 {
		return s[:i]
	}
	return s
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
