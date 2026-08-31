package core

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCheckPrefixUnknownPrefixWarnRateLimit(t *testing.T) {
	var buf bytes.Buffer
	n := testNylonWithPrefixes(staticDynPrefix(netip.MustParsePrefix("10.0.0.0/24"), 0))
	n.router.log = slog.New(slog.NewTextHandler(&buf, nil)) // same pattern as TestCheckPrefixDynamicRanges

	unknown := netip.MustParsePrefix("10.99.0.1/32")
	other := netip.MustParsePrefix("10.99.0.2/32")

	// repeated rejections of the same prefix warn exactly once per interval;
	// fail-closed rejection itself is unchanged
	for range 3 {
		assert.False(t, n.checkPrefix(unknown), "fail-closed rejection is unchanged")
	}
	assert.Equal(t, 1, strings.Count(buf.String(), "received packet for unknown prefix"))

	// a different prefix has its own budget
	assert.False(t, n.checkPrefix(other))
	assert.Equal(t, 2, strings.Count(buf.String(), "received packet for unknown prefix"))

	// once the interval elapses the warning is emitted again
	n.router.unknownPrefixWarns[unknown] = time.Now().Add(-2 * n.UnknownPrefixWarnInterval)
	assert.False(t, n.checkPrefix(unknown))
	assert.Equal(t, 3, strings.Count(buf.String(), "received packet for unknown prefix"))
}
