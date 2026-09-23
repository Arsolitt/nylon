package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/encodeous/nylon/perf"
)

// tunStatsPollInterval is how often the kernel TUN interface counters are sampled.
const tunStatsPollInterval = 10 * time.Second

// startTunStatsPolling samples the kernel interface counters into perf totals.
// It polls /sys/class/net/<Interface>/statistics/tx_dropped every
// tunStatsPollInterval until n.Context is cancelled, storing the value into
// perf.TunKernelTxDroppedTotal.
//
// The value is a sysfs sample, not an accumulated total: the kernel counter is
// monotonic only for the lifetime of the interface, so it resets to zero when
// the interface is recreated (and it is reported as the last successfully read
// sample). It is a no-op when there is no TUN interface (n.NoTun) and off Linux,
// where the sysfs path does not exist; read and parse failures are ignored.
func (n *Nylon) startTunStatsPolling() {
	if n.NoTun || n.Interface == "" {
		return
	}
	path := filepath.Join("/sys/class/net", n.Interface, "statistics", "tx_dropped")
	go func() {
		ticker := time.NewTicker(tunStatsPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-n.Context.Done():
				return
			case <-ticker.C:
			}
			value, err := readSysfsUint64(path)
			if err != nil {
				continue
			}
			perf.TunKernelTxDroppedTotal.Store(value)
		}
	}()
}

// readSysfsUint64 parses a newline-terminated decimal counter from a sysfs file.
func readSysfsUint64(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}
