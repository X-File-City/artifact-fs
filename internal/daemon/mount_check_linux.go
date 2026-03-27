package daemon

import (
	"os"
	"os/exec"
	"strings"
)

// isMounted checks whether the given path is an active mount point.
// Reads /proc/mounts first (fast, no subprocess), falls back to mount(8).
func isMounted(path string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return false
		}
		return strings.Contains(string(out), path)
	}
	return strings.Contains(string(data), path)
}
