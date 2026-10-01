package dockerbuild

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// alwaysExclude is prepended to every project's ignore rules.
//
// node_modules is the important one: the build stage does `COPY . .` on top
// of the node_modules the deps stage installed, so shipping the host's copy
// would overwrite a correctly-installed tree with whatever the uploader
// happened to have — quite possibly built for a different platform. .git is
// excluded because it's dead weight in an image and routinely holds
// credentials in its remote config.
var alwaysExclude = []string{
	"node_modules",
	"**/node_modules",
	".git",
	"**/.git",
}

// ── ignore matching ──────────────────────────────────────────────────────

type ignorePattern struct {
	segments []string
	negate   bool
}

type ignoreMatcher struct {
	patterns     []ignorePattern
	hasNegations bool
}

// loadIgnoreMatcher reads .dockerignore from the context root, if present.
// A missing file is not an error — it just means nothing beyond the
// always-excluded paths is filtered.
func loadIgnoreMatcher(root string) (*ignoreMatcher, error) {
	lines := append([]string{}, alwaysExclude...)

	data, err := os.ReadFile(filepath.Join(root, ".dockerignore"))
	if err == nil {
		lines = append(lines, strings.Split(string(data), "\n")...)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	m := &ignoreMatcher{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		negate := false
		if strings.HasPrefix(line, "!") {
			negate = true
			m.hasNegations = true
			line = strings.TrimPrefix(line, "!")
		}

		line = strings.TrimPrefix(path.Clean(strings.TrimPrefix(line, "./")), "/")
		if line == "" || line == "." {
			continue
		}

		m.patterns = append(m.patterns, ignorePattern{
			segments: strings.Split(line, "/"),
			negate:   negate,
		})
	}

	return m, nil
}

// matches reports whether rel (a slash-separated path relative to the
// context root) is excluded. Later patterns win over earlier ones, which is
// what makes a `!keep-me` line after a broad exclusion work.
func (m *ignoreMatcher) matches(rel string) bool {
	excluded := false
	segments := strings.Split(rel, "/")

	for _, p := range m.patterns {
		if matchSegments(p.segments, segments) {
			excluded = !p.negate
		}
	}

	return excluded
}

// canPrune reports whether an excluded directory can be skipped wholesale
// rather than walked. A negation anywhere in the rules could re-include
// something nested inside it, so pruning is only safe when there are none.
func (m *ignoreMatcher) canPrune(rel string) bool {
	return !m.hasNegations && m.matches(rel)
}

// matchSegments implements Docker's pattern semantics: a plain `*` matches
// within one path segment, while `**` spans any number of segments
// (including none).
func matchSegments(pattern, name []string) bool {
	if len(pattern) == 0 {
		return len(name) == 0
	}

	if pattern[0] == "**" {
		// try consuming zero or more segments with the rest of the pattern
		for i := 0; i <= len(name); i++ {
			if matchSegments(pattern[1:], name[i:]) {
				return true
			}
		}
		return false
	}

	if len(name) == 0 {
		return false
	}

	if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
		return false
	}

	// a directory pattern also excludes everything beneath it
	if len(pattern) == 1 {
		return true
	}

	return matchSegments(pattern[1:], name[1:])
}

// ── tar streaming ────────────────────────────────────────────────────────

// tarContext streams root as a tar archive, applying the ignore rules and
// appending the generated Dockerfile (when one was generated) as an extra
// entry. It streams through an io.Pipe rather than buffering, so a large
// project doesn't have to fit in memory twice on its way to the daemon.
//
// The returned reader must be fully consumed or closed, or the writing
// goroutine leaks.
func tarContext(root string, extraFiles map[string]string) (io.ReadCloser, error) {
	matcher, err := loadIgnoreMatcher(root)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()

	go func() {
		tw := tar.NewWriter(pw)

		walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == root {
				return nil
			}

			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			if d.IsDir() {
				if matcher.canPrune(rel) {
					return fs.SkipDir
				}
				// a matched-but-not-prunable directory still gets walked;
				// its excluded children are filtered individually below
				if matcher.matches(rel) {
					return nil
				}
			} else if matcher.matches(rel) {
				return nil
			}

			// extraFiles is authoritative for any path it names — e.g. a
			// lockfile rewritten to strip a private registry reference —
			// so the real file on disk is skipped rather than shipping
			// both and relying on tar's last-entry-wins extraction order
			if _, overridden := extraFiles[rel]; overridden {
				return nil
			}

			return writeTarEntry(tw, p, rel, d)
		})

		if walkErr == nil {
			walkErr = writeExtraFiles(tw, extraFiles)
		}
		if walkErr != nil {
			tw.Close()
			pw.CloseWithError(walkErr)
			return
		}

		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	return pr, nil
}

func writeTarEntry(tw *tar.Writer, fullPath, rel string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}

	link := ""
	if info.Mode()&os.ModeSymlink != 0 {
		if link, err = os.Readlink(fullPath); err != nil {
			return err
		}
	}

	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = rel

	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	// directories and symlinks carry no payload; anything that isn't a
	// regular file (sockets, devices, fifos) is skipped rather than
	// streamed — the daemon has no use for it and reading one can block
	if !info.Mode().IsRegular() {
		return nil
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(tw, f)
	return err
}

func writeExtraFiles(tw *tar.Writer, files map[string]string) error {
	for name, content := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0o600,
			Size: int64(len(content)),
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if _, err := io.WriteString(tw, content); err != nil {
			return err
		}
	}
	return nil
}
