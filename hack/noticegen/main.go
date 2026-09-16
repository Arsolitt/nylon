// Command noticegen regenerates NOTICE and licenses/ from the module graph of
// the released binaries.
//
// Run it from the repository root:
//
//	go run ./hack/noticegen
//
// The module cache must be warm (go mod download) because the sources of every
// dependency are read to collect the license files they ship.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// mainModule is the module the released binaries belong to; its packages are
	// not third-party components.
	mainModule = "github.com/encodeous/nylon"

	noticePath  = "NOTICE"
	licensesDir = "licenses"
)

// packages are the entry points GoReleaser builds and ships. ./cmd/nylon-lb is
// listed even though it is released as a container image only.
var packages = []string{"./cmd/nylon", "./cmd/nylon-genesis", "./cmd/nylon-lb"}

// target is one (GOOS, GOARCH) pair of the release matrix.
type target struct {
	goos   string
	goarch string
}

// targets mirrors the release matrix in .goreleaser.yaml (linux/386,
// linux/amd64, linux/arm64, darwin/amd64, darwin/arm64). Keep the two in sync:
// a target added there must be added here, otherwise components that only build
// for it are silently missing from NOTICE. Windows is never released
// (github.com/kmahyyg/go-network-compo is not open source), so it is not listed.
var targets = []target{
	{"linux", "386"},
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
}

// licensePrefixes are the lowercased file name prefixes that identify the
// attribution and license files an upstream module ships.
var licensePrefixes = []string{"license", "licence", "notice", "copying", "authors", "patents"}

// module is one third-party component of the dependency graph.
type module struct {
	path    string
	version string
	dir     string
}

// moduleFiles is one module together with the attribution files it ships.
type moduleFiles struct {
	names   []string // selected file names, sorted
	content map[string][]byte
}

// noticeHeader is reproduced verbatim at the top of NOTICE.
const noticeHeader = `nylon
=====

nylon is a self-healing WireGuard mesh. This file lists the third-party
components included in the released binaries and reproduces the attribution
notices they ship. The full license text of every component is in
licenses/<module>/ next to this file.

nylon itself is licensed under the Apache License 2.0 (LICENSE); polyamide/
is distributed under the MIT license (polyamide/LICENSE).

nylon-genesis binaries link github.com/Arsolitt/amnezigo, which is licensed
under the GNU General Public License v3.0 (LICENSE.GPL-3.0). Distributing a
nylon-genesis binary means passing on the GPL-3.0 terms together with the
corresponding source (this repository and github.com/Arsolitt/amnezigo).
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "noticegen:", err)
		os.Exit(1)
	}
}

func run() error {
	mods, err := collectModules()
	if err != nil {
		return err
	}

	paths := make([]string, 0, len(mods))
	for path := range mods {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	// Step 2: copy the attribution files of every component.
	files := make(map[string]*moduleFiles, len(paths))
	written := 0
	for _, path := range paths {
		m := mods[path]
		mf := &moduleFiles{content: map[string][]byte{}}
		if m.dir != "" {
			names, err := selectLicenseFiles(m.dir)
			if err != nil {
				return err
			}
			mf.names = names
			for _, name := range names {
				src := filepath.Join(m.dir, name)
				data, err := os.ReadFile(src)
				if err != nil {
					return fmt.Errorf("read %s: %w", src, err)
				}
				mf.content[name] = data

				dst := filepath.Join(licensesDir, filepath.FromSlash(path), name)
				if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
					return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
				}
				if err := os.WriteFile(dst, data, 0o644); err != nil {
					return fmt.Errorf("write %s: %w", dst, err)
				}
				written++
			}
		}
		files[path] = mf
	}

	// Step 3: write NOTICE.
	notice, notices := renderNotice(paths, mods, files)
	if err := os.WriteFile(noticePath, notice, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", noticePath, err)
	}

	// Step 4: report.
	fmt.Printf("noticegen: %d components, %d with NOTICE files, %d license files written\n",
		len(paths), notices, written)
	return nil
}

// collectModules resolves the dependency graph of every released target and
// unions the modules found into one map keyed by module path.
func collectModules() (map[string]module, error) {
	const format = "{{if .Module}}{{.Module.Path}}\t{{.Module.Version}}\t{{.Module.Dir}}{{end}}"

	mods := make(map[string]module)
	for _, t := range targets {
		cmd := exec.Command("go", append([]string{"list", "-deps", "-f", format}, packages...)...)
		cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch)

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("go list -deps for %s/%s: %w: %s",
				t.goos, t.goarch, err, strings.TrimSpace(stderr.String()))
		}

		for _, line := range strings.Split(stdout.String(), "\n") {
			if strings.TrimSpace(line) == "" {
				continue // stdlib packages have no module
			}
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) != 3 {
				return nil, fmt.Errorf("unexpected go list output line %q", line)
			}
			path, version, dir := fields[0], fields[1], fields[2]
			if path == mainModule || strings.HasPrefix(path, mainModule+"/") {
				continue
			}
			mods[path] = module{path: path, version: version, dir: dir}
		}
	}
	return mods, nil
}

// selectLicenseFiles returns the sorted names of the attribution files a module
// ships in its root directory.
func selectLicenseFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)
		for _, prefix := range licensePrefixes {
			if strings.HasPrefix(lower, prefix) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

// renderNotice composes NOTICE and reports how many components ship a NOTICE
// file. Everything is sorted so that repeated runs are byte-identical.
func renderNotice(paths []string, mods map[string]module, files map[string]*moduleFiles) ([]byte, int) {
	var b strings.Builder
	b.WriteString(noticeHeader)

	writeHeading(&b, "Bundled components")
	b.WriteString("\n")
	for _, path := range paths {
		m := mods[path]
		names := files[path].names
		list := "no license file provided by upstream"
		if len(names) > 0 {
			list = strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "%s %s — %s\n", m.path, m.version, list)
	}

	writeHeading(&b, "Third-party attribution notices")
	withNotice := 0
	for _, path := range paths {
		m := mods[path]
		mf := files[path]
		found := false
		for _, name := range mf.names {
			if !strings.HasPrefix(strings.ToLower(name), "notice") {
				continue
			}
			found = true
			fmt.Fprintf(&b, "\n## %s %s\n\n", m.path, m.version)
			content := mf.content[name]
			b.WriteString(string(content))
			if len(content) == 0 || content[len(content)-1] != '\n' {
				b.WriteString("\n")
			}
		}
		if found {
			withNotice++
		}
	}

	return []byte(strings.TrimRight(b.String(), "\n") + "\n"), withNotice
}

// writeHeading writes a section title underlined with the same convention as
// the file header.
func writeHeading(b *strings.Builder, title string) {
	b.WriteString("\n")
	b.WriteString(title)
	b.WriteString("\n")
	b.WriteString(strings.Repeat("=", len(title)))
	b.WriteString("\n")
}
