package daemon

import (
	"os/exec"
	"strings"
)

// isMounted checks whether the given path is an active mount point.
// On macOS, mount(8) reports /private/tmp even for /tmp paths.
func isMounted(path string) bool {
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, path) || strings.Contains(s, "/private"+path)
}
