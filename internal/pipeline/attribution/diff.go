package attribution

import (
	"strings"
)

// addedLines maps slash-normalized paths to new-file line numbers that a
// unified diff introduced. Context and deletion lines are not additions: a
// touched file is not evidence that every line in it is new.
func addedLines(diff string) map[string]map[int]struct{} {
	out := make(map[string]map[int]struct{})
	var path string
	newLine := 0
	for _, raw := range strings.Split(diff, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "+++ "):
			path = normalizeDiffPath(strings.TrimPrefix(line, "+++ "))
			newLine = 0
		case strings.HasPrefix(line, "@@ "):
			newLine = parseHunkNewStart(line)
		case path == "" || path == "/dev/null":
			continue
		case strings.HasPrefix(line, "+"):
			if strings.HasPrefix(line, "+++") {
				continue
			}
			if newLine > 0 {
				setAdded(out, path, newLine)
			}
			newLine++
		case strings.HasPrefix(line, "-"):
			if strings.HasPrefix(line, "---") {
				continue
			}
			// deletion: old file only
		case strings.HasPrefix(line, "\\"):
			// "\ No newline at end of file"
		default:
			if newLine > 0 {
				newLine++
			}
		}
	}
	return out
}

func setAdded(out map[string]map[int]struct{}, path string, line int) {
	lines, ok := out[path]
	if !ok {
		lines = make(map[int]struct{})
		out[path] = lines
	}
	lines[line] = struct{}{}
}

func normalizeDiffPath(p string) string {
	p = strings.TrimSpace(p)
	if tab := strings.IndexByte(p, '\t'); tab >= 0 {
		p = p[:tab]
	}
	p = strings.Trim(p, `"`)
	if strings.HasPrefix(p, "b/") || strings.HasPrefix(p, "a/") {
		p = p[2:]
	}
	return strings.ReplaceAll(p, "\\", "/")
}

func parseHunkNewStart(hunk string) int {
	// @@ -l,s +l,s @@
	plus := strings.Index(hunk, "+")
	if plus < 0 {
		return 0
	}
	rest := hunk[plus+1:]
	end := strings.IndexAny(rest, ", ")
	if end < 0 {
		end = len(rest)
	}
	n := 0
	for _, r := range rest[:end] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func lineAddedIn(added map[string]map[int]struct{}, file string, line int) bool {
	if file == "" || line <= 0 || added == nil {
		return false
	}
	file = strings.ReplaceAll(file, "\\", "/")
	if lines, ok := added[file]; ok {
		_, hit := lines[line]
		return hit
	}
	base := file
	if i := strings.LastIndex(file, "/"); i >= 0 {
		base = file[i+1:]
	}
	for path, lines := range added {
		if path == base || strings.HasSuffix(path, "/"+base) {
			_, hit := lines[line]
			if hit {
				return true
			}
		}
	}
	return false
}

func fileInDiff(added map[string]map[int]struct{}, file string) bool {
	if file == "" || added == nil {
		return false
	}
	file = strings.ReplaceAll(file, "\\", "/")
	if _, ok := added[file]; ok {
		return true
	}
	base := file
	if i := strings.LastIndex(file, "/"); i >= 0 {
		base = file[i+1:]
	}
	for path := range added {
		if path == base || strings.HasSuffix(path, "/"+base) {
			return true
		}
	}
	return false
}
