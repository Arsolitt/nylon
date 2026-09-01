package main

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
)

// lbPrefixFile mirrors the on-disk dyn-prefix schema parsed by the nylon
// daemon (design §4.2). The daemon-side structs (dynPrefixFile/dynPrefixEntry
// in core/dynamic_prefixes.go) are unexported, so they are duplicated here
// instead of imported; the golden-bytes test keeps the two in sync.
type lbPrefixFile struct {
	Version  int             `json:"version"` // 1
	Prefixes []lbPrefixEntry `json:"prefixes"`
}

// lbPrefixEntry mirrors the daemon-side entry schema. nylon-lb only ever
// writes static /32 host routes with metric 0.
type lbPrefixEntry struct {
	Type   string  `json:"type"`             // "static"
	Prefix string  `json:"prefix"`           // "<ip>/32", must be masked
	Metric *uint32 `json:"metric,omitempty"` // 0
}

// fileName returns the prefixes.d file name owning Service ns/name. k8s
// namespace and name are DNS-1123-safe, so no sanitization is needed. The
// lb- prefix is also the GC boundary: gcPrefixFiles only ever touches
// lb-*.json and never files owned by other tooling such as 00-vip.json or
// 10-podcidr.json.
func fileName(ns, name string) string {
	return "lb-" + ns + "-" + name + ".json"
}

// writePrefixFile writes the announce file for Service ns/name into dir
// (assumed to exist; the caller creates it), announcing ip as a single static
// /32 entry with metric 0. The content is compact JSON plus a trailing
// newline. If the existing file already holds the exact bytes, nothing is
// written (changed=false) so resyncs do not churn the daemon's watcher;
// otherwise the file is replaced atomically: write to a temp file (whose name
// must NOT end in .json — the daemon globs *.json), chmod 0644, then rename
// onto the final name. Rename-into-dir is the proven trigger for the daemon's
// fsnotify watcher; on any error the temp file is removed.
func writePrefixFile(dir, ns, name string, ip netip.Addr) (changed bool, err error) {
	metric := uint32(0)
	content, err := json.Marshal(lbPrefixFile{
		Version: 1,
		Prefixes: []lbPrefixEntry{{
			Type:   "static",
			Prefix: netip.PrefixFrom(ip, 32).Masked().String(),
			Metric: &metric,
		}},
	})
	if err != nil {
		return false, err
	}
	content = append(content, '\n')

	finalName := fileName(ns, name)
	finalPath := filepath.Join(dir, finalName)
	if existing, readErr := os.ReadFile(finalPath); readErr == nil && string(existing) == string(content) {
		return false, nil
	}

	tmp, err := os.CreateTemp(dir, finalName+".tmp")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if _, err = tmp.Write(content); err != nil {
		_ = tmp.Close()
		return false, err
	}
	if err = tmp.Close(); err != nil {
		return false, err
	}
	if err = os.Rename(tmpPath, finalPath); err != nil {
		return false, err
	}
	return true, nil
}

// gcPrefixFiles withdraws announces this controller no longer holds: every
// lb-*.json in dir whose base name is not in keep is unlinked, and nothing
// else is ever touched. Returns the sorted base names of removed files.
func gcPrefixFiles(dir string, keep map[string]struct{}) (removed []string, err error) {
	matches, err := filepath.Glob(filepath.Join(dir, "lb-*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range matches {
		base := filepath.Base(path)
		if _, ok := keep[base]; ok {
			continue
		}
		if rmErr := os.Remove(path); rmErr != nil {
			return removed, rmErr
		}
		removed = append(removed, base)
	}
	sort.Strings(removed)
	return removed, nil
}
