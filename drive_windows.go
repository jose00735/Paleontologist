//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

var getLogicalDrives = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalDrives")

// candidateLetters returns free drive letters, preferred first. Letters with a remembered
// (persistent but disconnected) network mapping are skipped: net use fails on them with error 1202.
func candidateLetters(want string) ([]string, error) {
	mask, _, _ := getLogicalDrives.Call()
	if want != "" {
		l := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(want), ":"))
		if len(l) != 1 || l[0] < 'A' || l[0] > 'Z' {
			return nil, fmt.Errorf("invalid drive letter %q", want)
		}
		if mask&(1<<(l[0]-'A')) != 0 {
			return nil, fmt.Errorf("drive %s: is already in use", l)
		}
		return []string{l}, nil
	}
	var out []string
	for c := 'Z'; c >= 'D'; c-- {
		if mask&(1<<(c-'A')) != 0 {
			continue
		}
		if _, err := run("reg", "query", `HKCU\Network\`+string(c)); err == nil {
			continue // remembered connection
		}
		out = append(out, string(c))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no free drive letters")
	}
	return out, nil
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func labelKey(port int) string {
	return fmt.Sprintf(`HKCU\Software\Microsoft\Windows\CurrentVersion\Explorer\MountPoints2\##localhost@%d#DavWWWRoot`, port)
}

var mappedPort int

func mapDriveLetter(want string, port int, label string) (string, error) {
	letters, err := candidateLetters(want)
	if err != nil {
		return "", err
	}
	// Friendly name shown in Explorer instead of "DavWWWRoot".
	run("reg", "add", labelKey(port), "/v", "_LabelFromReg", "/t", "REG_SZ", "/d", label+" (paleontologist)", "/f")
	mappedPort = port
	targets := []string{fmt.Sprintf("http://localhost:%d/", port), fmt.Sprintf(`\\localhost@%d\DavWWWRoot`, port)}
	var last string
	tries := 0
	for _, letter := range letters {
		if tries >= 6 {
			break
		}
		tries++
		letterBusy := false
		for _, t := range targets {
			out, err := run("net", "use", letter+":", t, "/persistent:no")
			if err == nil {
				return letter, nil
			}
			last = out
			if strings.Contains(out, "1202") || strings.Contains(out, " 85 ") || strings.Contains(out, "error 85") {
				letterBusy = true // letter taken or remembered: try the next one
				break
			}
			if strings.Contains(out, " 67 ") || strings.Contains(out, "error 67") {
				run("sc", "start", "WebClient")
			}
		}
		if !letterBusy {
			break
		}
	}
	return "", fmt.Errorf("net use failed: %s\n\nIf the message mentions the network name or WebClient, make sure the \"WebClient\"\nservice is running (services.msc -> WebClient -> Start); you can also pick a letter with -drive Y", last)
}

func unmapDriveLetter(letter string) {
	run("net", "use", letter+":", "/delete", "/y")
	if mappedPort != 0 {
		run("reg", "delete", labelKey(mappedPort), "/f")
	}
}

func openExplorer(path string) {
	exec.Command("explorer.exe", path).Start()
}

func fixWebDAVLimit() error {
	out, err := run("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters`,
		"/v", "FileSizeLimitInBytes", "/t", "REG_DWORD", "/d", "4294967295", "/f")
	if err != nil {
		return fmt.Errorf("%s\nRun this command from a terminal opened as Administrator", out)
	}
	fmt.Println("WebDAV file size limit raised to 4 GB (Windows' maximum).")
	run("net", "stop", "WebClient")
	if out, err := run("net", "start", "WebClient"); err != nil {
		fmt.Fprintln(os.Stderr, "Restart the WebClient service or reboot to apply:", out)
	} else {
		fmt.Println("WebClient service restarted. Mount the disk again.")
	}
	return nil
}
