package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// augmentedCLIEnv constructs a minimal, isolated environment whitelist for MCP client
// discovery and registration commands without forwarding arbitrary parent secrets.
func augmentedCLIEnv(home string) []string {
	var extra []string
	if home != "" {
		extra = append(extra,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		)
	}
	if runtime.GOOS == "darwin" {
		extra = append(extra, "/opt/homebrew/bin", "/usr/local/bin")
	} else if runtime.GOOS != "windows" {
		extra = append(extra, "/usr/local/bin", "/usr/bin", "/bin")
	}

	var allowKeys []string
	if runtime.GOOS == "windows" {
		allowKeys = []string{
			"USERPROFILE", "HOMEPATH", "HOMEDRIVE", "APPDATA", "LOCALAPPDATA",
			"TEMP", "TMP", "SystemRoot", "ComSpec", "OS", "PATHEXT", "LANG",
		}
	} else {
		allowKeys = []string{
			"USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE",
			"TERM", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		}
	}

	env := make([]string, 0, len(allowKeys)+2)
	if home != "" {
		env = append(env, "HOME="+home)
		if runtime.GOOS == "windows" {
			env = append(env, "USERPROFILE="+home)
		}
	} else if v := os.Getenv("HOME"); v != "" {
		env = append(env, "HOME="+v)
	}

	for _, key := range allowKeys {
		if key == "USERPROFILE" && home != "" {
			continue
		}
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}

	prefix := strings.Join(extra, string(os.PathListSeparator))
	currPath := os.Getenv("PATH")
	var mergedPath string
	switch {
	case prefix != "" && currPath != "":
		mergedPath = prefix + string(os.PathListSeparator) + currPath
	case prefix != "":
		mergedPath = prefix
	default:
		mergedPath = currPath
	}
	env = append(env, "PATH="+mergedPath)
	return env
}

func lookupAgentCLIBinary(home, name string) string {
	candidates := make([]string, 0, 5)
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", name),
			filepath.Join(home, "bin", name),
		)
	}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, filepath.Join("/opt/homebrew/bin", name), filepath.Join("/usr/local/bin", name))
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			if runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0 {
				return p
			}
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found
	}
	return ""
}
