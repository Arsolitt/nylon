package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/encodeous/nylon/state"
	"github.com/fsnotify/fsnotify"
)

// dynamicPrefixDebounce is how long the prefixes.d watcher waits after the
// last filesystem event before rescanning the directory (design §4.5).
const dynamicPrefixDebounce = 250 * time.Millisecond

// dynPrefixFile is the on-disk schema (design §4.2) of one prefixes.d file.
type dynPrefixFile struct {
	Version  int              `json:"version"` // must be 1
	Prefixes []dynPrefixEntry `json:"prefixes"`
}

// dynPrefixEntry mirrors the health-config structs in state/prefix_health.go.
type dynPrefixEntry struct {
	Type        string  `json:"type"`                   // static | ping | http
	Prefix      string  `json:"prefix"`                 // netip.ParsePrefix, must be masked
	Metric      *uint32 `json:"metric,omitempty"`       // static: value; ping/http: override
	Addr        string  `json:"addr,omitempty"`         // ping only
	MaxFailures *int    `json:"max_failures,omitempty"` // ping only
	Delay       string  `json:"delay,omitempty"`        // ping/http; Go duration string
	BindIf      string  `json:"bind_if,omitempty"`      // ping only
	URL         string  `json:"url,omitempty"`          // http only
}

// parseDynamicPrefixFile converts one prefixes.d file into health wrappers.
// Structural problems (JSON syntax error, unsupported version) skip the whole
// file and are returned as errors; per-entry problems skip only that entry.
func parseDynamicPrefixFile(name string, data []byte) ([]state.PrefixHealthWrapper, []error) {
	var file dynPrefixFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, []error{fmt.Errorf("%s: skipping file: %w", name, err)}
	}
	if file.Version != 1 {
		return nil, []error{fmt.Errorf("%s: skipping file: unsupported version %d", name, file.Version)}
	}
	entries := make([]state.PrefixHealthWrapper, 0, len(file.Prefixes))
	var errs []error
	for i, entry := range file.Prefixes {
		wrapper, err := entry.toWrapper()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: entry %d: %w", name, i, err))
			continue
		}
		entries = append(entries, wrapper)
	}
	return entries, errs
}

func (e dynPrefixEntry) toWrapper() (state.PrefixHealthWrapper, error) {
	prefix, err := netip.ParsePrefix(e.Prefix)
	if err != nil {
		return state.PrefixHealthWrapper{}, fmt.Errorf("invalid prefix %q: %w", e.Prefix, err)
	}
	if prefix != prefix.Masked() {
		return state.PrefixHealthWrapper{}, fmt.Errorf("prefix %s has host bits set", prefix)
	}
	switch e.Type {
	case "static":
		var metric uint32
		if e.Metric != nil {
			metric = *e.Metric
		}
		return state.PrefixHealthWrapper{PrefixHealth: &state.StaticPrefixHealth{
			Prefix: prefix,
			Metric: metric,
		}}, nil
	case "ping":
		if e.Addr == "" {
			return state.PrefixHealthWrapper{}, errors.New(`ping entry requires "addr"`)
		}
		addr, err := netip.ParseAddr(e.Addr)
		if err != nil {
			return state.PrefixHealthWrapper{}, fmt.Errorf("invalid addr %q: %w", e.Addr, err)
		}
		delay, err := parseDynDelay(e.Delay)
		if err != nil {
			return state.PrefixHealthWrapper{}, err
		}
		return state.PrefixHealthWrapper{PrefixHealth: &state.PingPrefixHealth{
			Prefix:      prefix,
			Addr:        addr,
			MaxFailures: e.MaxFailures,
			Delay:       delay,
			BindIf:      e.BindIf,
			Metric:      e.Metric,
		}}, nil
	case "http":
		if e.URL == "" {
			return state.PrefixHealthWrapper{}, errors.New(`http entry requires "url"`)
		}
		delay, err := parseDynDelay(e.Delay)
		if err != nil {
			return state.PrefixHealthWrapper{}, err
		}
		return state.PrefixHealthWrapper{PrefixHealth: &state.HTTPPrefixHealth{
			Prefix: prefix,
			URL:    e.URL,
			Delay:  delay,
			Metric: e.Metric,
		}}, nil
	default:
		return state.PrefixHealthWrapper{}, fmt.Errorf("unknown type %q", e.Type)
	}
}

func parseDynDelay(value string) (*time.Duration, error) {
	if value == "" {
		return nil, nil
	}
	delay, err := time.ParseDuration(value)
	if err != nil {
		return nil, fmt.Errorf("invalid delay %q: %w", value, err)
	}
	return &delay, nil
}

// scanDynamicPrefixDir returns the union of valid entries from all *.json
// files in dir, in lexicographic filename order. A duplicate prefix across
// files keeps the entry from the earlier file (design §4.4).
func scanDynamicPrefixDir(log *slog.Logger, dir string) []state.PrefixHealthWrapper {
	dirents, err := os.ReadDir(dir)
	if err != nil {
		log.Error("cannot read dynamic prefixes dir", "dir", dir, "err", err.Error())
		return nil
	}
	var result []state.PrefixHealthWrapper
	seen := make(map[netip.Prefix]string)
	for _, dirent := range dirents {
		if dirent.IsDir() || !strings.HasSuffix(dirent.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, dirent.Name()))
		if err != nil {
			log.Error("cannot read dynamic prefix file", "file", dirent.Name(), "err", err.Error())
			continue
		}
		entries, errs := parseDynamicPrefixFile(dirent.Name(), data)
		for _, parseErr := range errs {
			log.Error("invalid dynamic prefix file", "err", parseErr.Error())
		}
		for _, entry := range entries {
			prefix := entry.GetPrefix()
			if keptFrom, dup := seen[prefix]; dup {
				log.Warn("duplicate dynamic prefix across files; keeping the entry from the earlier file",
					"prefix", prefix.String(), "kept_from", keptFrom, "dropped_from", dirent.Name())
				continue
			}
			seen[prefix] = dirent.Name()
			result = append(result, entry)
		}
	}
	return result
}

// loadDynamicPrefixesDir rescans the dynamic prefixes directory into
// n.dynamicPrefixes.
func (n *Nylon) loadDynamicPrefixesDir() {
	n.dynamicPrefixes = scanDynamicPrefixDir(n.Log, n.LocalCfg.DynamicPrefixesDir)
}

// injectDynamicPrefixes appends the local dynamic prefixes to the local
// node's entry in cfg. Prefixes already present in the central config win
// (design §4.4). A nil local node entry is a no-op.
func (n *Nylon) injectDynamicPrefixes(cfg *state.CentralCfg) {
	node := cfg.TryGetNode(n.LocalCfg.Id)
	if node == nil {
		return
	}
	central := make(map[netip.Prefix]struct{}, len(node.Prefixes))
	for _, existing := range node.Prefixes {
		central[existing.GetPrefix()] = struct{}{}
	}
	for _, entry := range n.dynamicPrefixes {
		prefix := entry.GetPrefix()
		if _, ok := central[prefix]; ok {
			n.Log.Error("dynamic prefix conflicts with central config; rejecting entry", "prefix", prefix.String())
			continue
		}
		central[prefix] = struct{}{}
		node.Prefixes = append(node.Prefixes, entry)
	}
}

// watchDynamicPrefixes watches dir for changes and, after a debounce,
// rescans it and re-applies the pristine central config (which re-injects the
// fresh dynamic set) on the main loop. Must run as its own goroutine; exits
// when the nylon context is cancelled, the watched directory is removed, or
// the watcher cannot be created (design §4.5).
func (n *Nylon) watchDynamicPrefixes(dir string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		n.Log.Error("cannot create dynamic prefixes watcher; feature disabled", "err", err.Error())
		return
	}
	defer watcher.Close()
	if err := watcher.Add(dir); err != nil {
		n.Log.Error("cannot watch dynamic prefixes dir; feature disabled", "dir", dir, "err", err.Error())
		return
	}
	n.Log.Info("watching dynamic prefixes dir", "dir", dir)

	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		var fire <-chan time.Time
		if timer != nil {
			fire = timer.C
		}
		select {
		case <-n.Context.Done():
			return
		case <-fire:
			timer = nil
			n.Dispatch(func() error {
				n.loadDynamicPrefixesDir()
				if n.centralCfgPristine != nil {
					if _, err := n.ApplyCentralConfig(n.centralCfgPristine); err != nil {
						n.Log.Warn("dynamic prefix re-apply incomplete; will retry on next event", "err", err.Error())
					}
				}
				return nil // never cancels the main loop
			})
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Remove) && filepath.Clean(event.Name) == filepath.Clean(dir) {
				n.Log.Error("dynamic prefixes dir removed; watcher stopped", "dir", dir)
				return
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.NewTimer(dynamicPrefixDebounce)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			n.Log.Error("dynamic prefixes watcher error", "err", err.Error())
		}
	}
}
