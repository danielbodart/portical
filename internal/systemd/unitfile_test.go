package systemd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func assertEqual[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestAssignmentsFindsTheKeyInEachSectionWithItsLine(t *testing.T) {
	text := strings.Join([]string{
		"[Unit]",
		"Description=Minecraft",
		"X-Portical-Forward=25565",
		"",
		"[Service]",
		"ExecStart=/bin/minecraft",
		"X-Portical-Forward = 19132/udp ",
	}, "\n")
	assertEqual(t, assignments(text, ForwardKey), []assignment{
		{section: "Unit", line: 3, value: "25565"},
		{section: "Service", line: 7, value: "19132/udp"},
	})
}

func TestAssignmentsSkipsBothKindsOfComment(t *testing.T) {
	text := "[Unit]\n# X-Portical-Forward=1\n; X-Portical-Forward=2\n  #X-Portical-Forward=3\n"
	assertEqual(t, assignments(text, ForwardKey), nil)
}

func TestAssignmentsJoinsContinuedLinesWithASpaceSkippingCommentsInsideThem(t *testing.T) {
	text := "[Unit]\nX-Portical-Forward=25565,\\\n# a comment\n  19132/udp\nAfter=x\n"
	assertEqual(t, assignments(text, ForwardKey), []assignment{{section: "Unit", line: 2, value: "25565, 19132/udp"}})
}

func TestAssignmentsReadsWindowsLineEndings(t *testing.T) {
	assertEqual(t, assignments("[Unit]\r\nX-Portical-Forward=1\r\n", ForwardKey), []assignment{{section: "Unit", line: 2, value: "1"}})
}

func TestAssignmentsSkipsAByteOrderMarkAsSystemdDoes(t *testing.T) {
	assertEqual(t, assignments("\uFEFF[Unit]\nX-Portical-Forward=1\n", ForwardKey), []assignment{{section: "Unit", line: 2, value: "1"}})
}

func TestAssignmentsIsCaseSensitiveAboutTheKeyAsSystemdIs(t *testing.T) {
	assertEqual(t, assignments("[Unit]\nx-portical-forward=1\n", ForwardKey), nil)
}

func TestAssignmentsKeepsAnEmptyAssignmentWhichMeansReset(t *testing.T) {
	assertEqual(t, assignments("[Unit]\nX-Portical-Forward=\n", ForwardKey), []assignment{{section: "Unit", line: 2, value: ""}})
}

func file(path, text string) unitFile { return unitFile{path: path, text: text} }

func declared(unit string, files ...unitFile) string {
	value, _ := declaration(unit, files, ForwardKey)
	return value
}

func TestDeclarationReadsTheUnitFile(t *testing.T) {
	assertEqual(t, declared("minecraft.service", file("/u", "[Unit]\nX-Portical-Forward=25565\n")), "25565")
}

func TestDeclarationAcceptsTheUnitTypesOwnSection(t *testing.T) {
	assertEqual(t, declared("minecraft.service", file("/u", "[Service]\nX-Portical-Forward=25565\n")), "25565")
	assertEqual(t, declared("dns.socket", file("/u", "[Socket]\nX-Portical-Forward=53\n")), "53")
}

func TestDeclarationIgnoresAnotherSectionAndSaysWhere(t *testing.T) {
	value, warnings := declaration("minecraft.service", []unitFile{file("/etc/x.service", "[Install]\nX-Portical-Forward=25565\n")}, ForwardKey)
	assertEqual(t, value, "")
	assertEqual(t, warnings, []string{"ignoring X-Portical-Forward in [Install] at /etc/x.service:2 - put it in [Unit] or [Service]"})
}

func TestDeclarationOnlySuggestsUnitForATypeWithNoSectionOfItsOwn(t *testing.T) {
	_, warnings := declaration("multi-user.target", []unitFile{file("/t", "X-Portical-Forward=1\n")}, ForwardKey)
	assertEqual(t, warnings, []string{"ignoring X-Portical-Forward in [] at /t:1 - put it in [Unit]"})
}

func TestDeclarationAddsEveryAssignmentInTheUnitFileAndItsDropIns(t *testing.T) {
	assertEqual(t, declared("minecraft.service",
		file("/u", "[Unit]\nX-Portical-Forward=25565\nX-Portical-Forward=25575/tcp\n"),
		file("/u.d/a.conf", "[Unit]\nX-Portical-Forward=19132/udp\n"),
	), "25565,25575/tcp,19132/udp")
}

func TestDeclarationLetsADropInTurnAUnitsForwardsOff(t *testing.T) {
	assertEqual(t, declared("minecraft.service",
		file("/u", "[Unit]\nX-Portical-Forward=25565\n"),
		file("/u.d/off.conf", "[Unit]\nX-Portical-Forward=\n"),
	), "")
}

func TestDeclarationLetsADropInReplaceAUnitsForwards(t *testing.T) {
	assertEqual(t, declared("minecraft.service",
		file("/u", "[Unit]\nX-Portical-Forward=25565\n"),
		file("/u.d/mine.conf", "[Unit]\nX-Portical-Forward=\nX-Portical-Forward=25566:25565/tcp\n"),
	), "25566:25565/tcp")
}

func TestDeclarationAppliesTheFilesInOrderSoALaterResetOnlyClearsWhatCameBefore(t *testing.T) {
	assertEqual(t, declared("minecraft.service",
		file("/u.d/a.conf", "[Unit]\nX-Portical-Forward=1\n"),
		file("/u.d/b.conf", "[Unit]\nX-Portical-Forward=\nX-Portical-Forward=2\n"),
		file("/u.d/c.conf", "[Unit]\nX-Portical-Forward=3\n"),
	), "2,3")
}

func TestDeclarationReadsARenamedKey(t *testing.T) {
	value, _ := declaration("minecraft.service", []unitFile{file("/u", "[Unit]\nX-Forward=80\n")}, "X-Forward")
	assertEqual(t, value, "80")
}

// The real file system, as the source reads it: os.DirFS("/") and absolute
// paths from systemd.

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readOK(t *testing.T, files *unitFiles, path string) string {
	t.Helper()
	text, err := files.read(path)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestUnitFilesReadAUnitFileAndSeeItChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mc.service")
	write(t, path, "[Unit]\nX-Portical-Forward=25565\n")
	files := newUnitFiles(os.DirFS("/"))
	assertEqual(t, readOK(t, files, path), "[Unit]\nX-Portical-Forward=25565\n")
	write(t, path, "[Unit]\nX-Portical-Forward=25566/tcp\n")
	assertEqual(t, readOK(t, files, path), "[Unit]\nX-Portical-Forward=25566/tcp\n")
}

func TestUnitFilesServeAnUnchangedFileFromTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mc.service")
	write(t, path, "[Unit]\nX-Portical-Forward=25565\n")
	files := newUnitFiles(os.DirFS("/"))
	readOK(t, files, path)
	// Unreadable now, so only the cache can answer.
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, readOK(t, files, path), "[Unit]\nX-Portical-Forward=25565\n")
}

func TestUnitFilesReadAFileDeletedSinceSystemdLoadedItAsEmptyRatherThanFailingEveryPass(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "mc.service")
	write(t, path, "[Unit]\nX-Portical-Forward=25565\n")
	files := newUnitFiles(os.DirFS("/"))
	readOK(t, files, path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, readOK(t, files, path), "")
	assertEqual(t, readOK(t, files, filepath.Join(directory, "gone.service.d", "forward.conf")), "")
	write(t, path, "x")
	assertEqual(t, readOK(t, files, filepath.Join(path, "under-a-file.conf")), "")
}

func TestUnitFilesStillFailOnAFileTheyMayNotRead(t *testing.T) {
	// A directory where a file should be: there to stat, not to read.
	if _, err := newUnitFiles(os.DirFS("/")).read(t.TempDir()); err == nil {
		t.Fatal("want an error")
	}
}
