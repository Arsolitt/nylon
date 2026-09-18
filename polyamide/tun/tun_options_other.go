//go:build !linux

package tun

// CreateTUNWithOptions creates a Device with the provided name and MTU. The
// queue options are Linux-specific; every other platform uses a single queue.
func CreateTUNWithOptions(name string, mtu int, opts CreateOptions) (Device, error) {
	return CreateTUN(name, mtu)
}
