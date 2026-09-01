package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWritePrefixFileGoldenBytes(t *testing.T) {
	dir := t.TempDir()

	changed, err := writePrefixFile(dir, "default", "web", netip.MustParseAddr("10.110.0.50"))
	require.NoError(t, err)
	assert.True(t, changed)

	got, err := os.ReadFile(filepath.Join(dir, "lb-default-web.json"))
	require.NoError(t, err)
	assert.Equal(t,
		[]byte("{\"version\":1,\"prefixes\":[{\"type\":\"static\",\"prefix\":\"10.110.0.50/32\",\"metric\":0}]}\n"),
		got)
}

func TestWritePrefixFileIdempotent(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "lb-default-web.json")

	_, err := writePrefixFile(dir, "default", "web", netip.MustParseAddr("10.110.0.50"))
	require.NoError(t, err)
	first, err := os.ReadFile(finalPath)
	require.NoError(t, err)

	// Same IP: no-op, bytes untouched.
	changed, err := writePrefixFile(dir, "default", "web", netip.MustParseAddr("10.110.0.50"))
	require.NoError(t, err)
	assert.False(t, changed)
	second, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	// Different IP: rewritten with updated bytes.
	changed, err = writePrefixFile(dir, "default", "web", netip.MustParseAddr("10.110.0.51"))
	require.NoError(t, err)
	assert.True(t, changed)
	third, err := os.ReadFile(finalPath)
	require.NoError(t, err)
	assert.Equal(t,
		[]byte("{\"version\":1,\"prefixes\":[{\"type\":\"static\",\"prefix\":\"10.110.0.51/32\",\"metric\":0}]}\n"),
		third)
}

func TestWritePrefixFileLeavesNoTempResidue(t *testing.T) {
	dir := t.TempDir()

	_, err := writePrefixFile(dir, "default", "web", netip.MustParseAddr("10.110.0.50"))
	require.NoError(t, err)
	_, err = writePrefixFile(dir, "default", "web", netip.MustParseAddr("10.110.0.51"))
	require.NoError(t, err)

	matches, err := filepath.Glob(filepath.Join(dir, "*"))
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dir, "lb-default-web.json")}, matches)
}

func TestGCPrefixFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"lb-stale.json", "lb-keep.json", "00-vip.json", "10-podcidr.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644))
	}

	removed, err := gcPrefixFiles(dir, map[string]struct{}{"lb-keep.json": {}})
	require.NoError(t, err)
	assert.Equal(t, []string{"lb-stale.json"}, removed)

	for _, name := range []string{"lb-keep.json", "00-vip.json", "10-podcidr.json"} {
		_, statErr := os.Stat(filepath.Join(dir, name))
		assert.NoError(t, statErr, "file %s must survive GC", name)
	}
	_, statErr := os.Stat(filepath.Join(dir, "lb-stale.json"))
	assert.True(t, os.IsNotExist(statErr), "lb-stale.json must be removed")
}
