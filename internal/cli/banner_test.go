package cli

import (
	"runtime"
	"strings"
	"testing"
)

// The banner is the breadcrumb for a start-up crash (issue #9): if it never
// appears, Portical's code did not run. So it has to name the build, the
// runtime and the machine, and it has to stay legible even when handed the
// base64 payload the internal `relay` command receives.

func TestBannerNamesTheBuildTheRuntimeAndTheMachine(t *testing.T) {
	line := Banner([]string{"run", "-f"}, "6.18.57")
	for _, want := range []string{
		"Portical ",
		"Go " + strings.TrimPrefix(runtime.Version(), "go"),
		runtime.GOOS + "/" + runtime.GOARCH,
		"kernel 6.18.57",
		"args: run -f",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("%q does not say %q", line, want)
		}
	}
}

func TestBannerSaysSoPlainlyWhenThereAreNoArguments(t *testing.T) {
	if line := Banner(nil, "6.18.57"); !strings.Contains(line, "args: (none)") {
		t.Fatal(line)
	}
}

func TestBannerCapsAHugeArgumentRatherThanDumpingTheWholeThing(t *testing.T) {
	line := Banner([]string{strings.Repeat("x", 5000)}, "6.18.57")
	if len(line) >= 400 || !strings.Contains(line, "(5000 chars)") {
		t.Fatal(line)
	}
}

func TestBannerNeverCutsACharacterInHalf(t *testing.T) {
	line := Banner([]string{strings.Repeat("é", 300)}, "6.18.57")
	if !strings.Contains(line, strings.Repeat("é", 200)+"... (300 chars)") {
		t.Fatal(line)
	}
}

func TestReleaseIsTheRunningKernels(t *testing.T) {
	if got := release(); got == "" || strings.ContainsAny(got, " \n") {
		t.Fatalf("%q", got)
	}
}
