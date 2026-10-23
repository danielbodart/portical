package cli

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/danielbodart/portical/internal/version"
)

// Banner is the first thing Main prints, and deliberately full of detail.
//
// Issue #9 was a start-up crash where the runtime's own crash report was all
// that appeared - and even its "Args:" line came out blank - so there was no
// way to tell whether any of Portical's own code had run, let alone how far it
// got or on what. This line answers that. If it shows up, Portical started and
// the build, the Go runtime, the machine and the arguments can be read
// straight from it; if it does not, the failure is underneath Portical, in the
// environment. Either way the next report says something the last one could
// not.
//
// The arguments are capped rather than printed whole: `relay` is handed a
// base64 SOAP body that can run to kilobytes, and the point here is a legible
// breadcrumb, not a transcript.
func Banner(args []string, kernel string) string {
	shown := "(none)"
	if len(args) > 0 {
		shown = strings.Join(args, " ")
	}
	if length := utf8.RuneCountInString(shown); length > 200 {
		shown = fmt.Sprintf("%s... (%d chars)", string([]rune(shown)[:200]), length)
	}
	return fmt.Sprintf("Portical %s starting - Go %s, %s/%s, kernel %s, args: %s",
		version.Version, strings.TrimPrefix(runtime.Version(), "go"), runtime.GOOS, runtime.GOARCH, kernel, shown)
}

// release is the running kernel's version, as `uname -r` prints it.
//
// Read from /proc rather than with uname(2), whose answer comes back as a
// fixed array of C chars to be trimmed by hand, and which is all this needs.
func release() string {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(data))
}
