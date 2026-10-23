package systemd

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

type unitFile struct {
	path, text string
}

// typeSections is the section each unit type keeps its own settings in, by
// unit suffix.
var typeSections = map[string]string{
	"service":   "Service",
	"socket":    "Socket",
	"mount":     "Mount",
	"automount": "Automount",
	"swap":      "Swap",
	"path":      "Path",
	"timer":     "Timer",
	"slice":     "Slice",
	"scope":     "Scope",
}

// suffix is a unit name's type: "service" for sshd.service.
func suffix(unit string) string {
	return unit[strings.LastIndex(unit, ".")+1:]
}

// declaration is the key's value once every file has been applied, as one
// label value, and what was wrong on the way.
//
// Read as systemd reads a list setting such as `Environment=`: each
// assignment adds to what came before, across the unit file and every
// drop-in, and an empty assignment throws away everything before it. So a
// drop-in can add a forward to a packaged unit, or turn its forwards off with
// a bare `X-Portical-Forward=`.
//
// Empty when nothing is declared, or everything declared was reset.
func declaration(unit string, files []unitFile, key string) (string, []string) {
	own := typeSections[suffix(unit)]
	var warnings, terms []string

	for _, file := range files {
		for _, found := range assignments(file.text, key) {
			if found.section != "Unit" && (own == "" || found.section != own) {
				hint := "[Unit]"
				if own != "" {
					hint += " or [" + own + "]"
				}
				warnings = append(warnings, fmt.Sprintf("ignoring %s in [%s] at %s:%d - put it in %s",
					key, found.section, file.path, found.line, hint))
				continue
			}
			if found.value == "" {
				terms = nil
			} else {
				terms = append(terms, found.value)
			}
		}
	}

	return strings.Join(terms, ","), warnings
}

type assignment struct {
	// "" before the first section header.
	section string
	line    int
	value   string
}

// assignments is every assignment of key in a unit file, top to bottom.
//
// As much of systemd's syntax as this needs: sections, `#` and `;` comments,
// and a trailing backslash continuing the line - joined with a space,
// skipping comment lines inside the continuation, as systemd does. Specifiers
// such as `%i` are not expanded; one in a value is reported by the rule parser
// as an unrecognised rule.
func assignments(text, key string) []assignment {
	var found []assignment
	// systemd skips a UTF-8 byte order mark, which some editors write, and
	// TrimSpace does not: left in, it hides the first section header.
	lines := strings.Split(strings.TrimPrefix(text, "\uFEFF"), "\n")
	section := ""

	for index := 0; index < len(lines); index++ {
		start := index + 1
		line := strings.TrimSpace(lines[index])
		if line == "" || comment(line) {
			continue
		}

		for strings.HasSuffix(line, `\`) && index+1 < len(lines) {
			line = strings.TrimSuffix(line, `\`)
			index++
			next := strings.TrimSpace(lines[index])
			if comment(next) {
				// A comment inside a continuation is dropped, and the line
				// before it still continues.
				line += `\`
				continue
			}
			line += " " + next
		}
		line = strings.TrimSuffix(line, `\`)

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}

		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		found = append(found, assignment{section: section, line: start, value: strings.TrimSpace(value)})
	}

	return found
}

func comment(line string) bool {
	return strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";")
}

// unitFiles reads unit files, cached by inode, size and modification time.
//
// A pass reads every running unit's files, and almost none of them change
// between passes, so this keeps a pass to a stat per file. A file system that
// cannot say what inode a file is - a test's map of files - is read every
// time, as a stamp without one could miss an edit that kept the size.
type unitFiles struct {
	fsys  fs.FS
	mu    sync.Mutex
	cache map[string]cachedFile
}

type cachedFile struct {
	inode    uint64
	size     int64
	modified time.Time
	text     string
}

func newUnitFiles(fsys fs.FS) *unitFiles {
	return &unitFiles{fsys: fsys, cache: map[string]cachedFile{}}
}

// read is the text of the file at an absolute path.
//
// A file that is not there reads as empty. systemd still names a file deleted
// since it was loaded, until the next daemon-reload, and what is on disk now
// is nothing, so it declares nothing - failing instead would fail every pass,
// for every source, until someone reloads. Any other error still fails the
// pass.
func (u *unitFiles) read(name string) (string, error) {
	relative := strings.TrimPrefix(path.Clean("/"+name), "/")
	info, err := fs.Stat(u.fsys, relative)
	if err != nil {
		if missing(err) {
			u.forget(name)
			return "", nil
		}
		return "", err
	}

	stat, cacheable := info.Sys().(*syscall.Stat_t)
	if cacheable {
		u.mu.Lock()
		cached, ok := u.cache[name]
		u.mu.Unlock()
		if ok && cached.inode == stat.Ino && cached.size == info.Size() && cached.modified.Equal(info.ModTime()) {
			return cached.text, nil
		}
	}

	data, err := fs.ReadFile(u.fsys, relative)
	if err != nil {
		// Deleted between the stat and the read.
		if missing(err) {
			u.forget(name)
			return "", nil
		}
		return "", err
	}
	if cacheable {
		u.mu.Lock()
		u.cache[name] = cachedFile{inode: stat.Ino, size: info.Size(), modified: info.ModTime(), text: string(data)}
		u.mu.Unlock()
	}
	return string(data), nil
}

func (u *unitFiles) forget(name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.cache, name)
}

// missing is whether err means there is no file there: not there at all, or
// something on the way to it is a file rather than a directory.
func missing(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}
