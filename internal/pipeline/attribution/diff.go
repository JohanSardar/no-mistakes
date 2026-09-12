package attribution

import (
	"context"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// diffAdded returns the lines from..to introduced, keyed by repo-relative path
// in to's coordinates. The command pins the unified-diff shape the parser
// reads: the maintainer's own git config (an external diff driver, mnemonic
// or no prefixes, forced color, quoted non-ASCII paths, textconv) must not
// change what counts as an added line.
func diffAdded(ctx context.Context, dir, from, to string) (map[string]map[int]struct{}, error) {
	out, err := git.Run(ctx, dir,
		"-c", "core.quotePath=false",
		"diff", "--no-color", "--no-ext-diff", "--no-textconv", "-M",
		"--src-prefix=a/", "--dst-prefix=b/",
		from, to,
	)
	if err != nil {
		return nil, err
	}
	return addedLines(out), nil
}

// addedLines maps slash-normalized paths to new-file line numbers that a
// unified diff introduced. Context and deletion lines are not additions: a
// touched file is not evidence that every line in it is new. File headers
// are read only between a "diff --git" line and its first hunk; inside a
// hunk every "+" line is content, so "+++count;" is an addition, not a header.
func addedLines(diff string) map[string]map[int]struct{} {
	out := make(map[string]map[int]struct{})
	var path string
	newLine := 0
	inHunk := false
	for _, raw := range strings.Split(diff, "\n") {
		line := strings.TrimRight(raw, "\r")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path, newLine, inHunk = "", 0, false
		case !inHunk && strings.HasPrefix(line, "+++ "):
			path = normalizeDiffPath(strings.TrimPrefix(line, "+++ "))
		case strings.HasPrefix(line, "@@ "):
			newLine = parseHunkNewStart(line)
			inHunk = true
		case !inHunk || path == "" || path == "/dev/null":
			continue
		case strings.HasPrefix(line, "+"):
			if newLine > 0 {
				setAdded(out, path, newLine)
			}
			newLine++
		case strings.HasPrefix(line, "-"):
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

// lineAddedIn matches the finding's repo-relative path exactly. A basename
// match is not identity: same-named files in different directories are the
// norm, and matching one would attribute a bug to a file nobody touched.
func lineAddedIn(added map[string]map[int]struct{}, file string, line int) bool {
	if file == "" || line <= 0 || added == nil {
		return false
	}
	file = strings.TrimPrefix(strings.ReplaceAll(file, "\\", "/"), "./")
	lines, ok := added[file]
	if !ok {
		return false
	}
	_, hit := lines[line]
	return hit
}
