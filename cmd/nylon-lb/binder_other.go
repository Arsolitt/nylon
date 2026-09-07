//go:build !linux

package main

import "fmt"

// newNetlinkBinder refuses construction off Linux: the netlink-backed binder
// is a Linux-only facility. Failing loudly keeps a misconfigured non-Linux
// binary from silently announcing addresses it cannot bind.
func newNetlinkBinder(linkName string, pools *PoolSet) (AddrBinder, error) {
	return nil, fmt.Errorf("netlink binder requires Linux (bind interface %q)", linkName)
}
