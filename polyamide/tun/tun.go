/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package tun

import (
	"os"
	"runtime"
)

type Event int

const (
	EventUp = 1 << iota
	EventDown
	EventMTUUpdate
)

// CreateOptions controls the Linux TUN link parameters at creation time.
type CreateOptions struct {
	Queues       int  // 1 = single queue; >1 attaches IFF_MULTI_QUEUE queues (Linux only)
	TxQueueLen   int  // 0 = keep the kernel default (500)
	Backpressure bool // request IFF_BACKPRESSURE; silently inactive on kernels that lack it
}

// DefaultCreateOptions returns the TUN link options used by CreateTUN.
func DefaultCreateOptions() CreateOptions {
	queues := runtime.NumCPU()
	if queues > 4 {
		queues = 4
	}
	if queues < 1 {
		queues = 1
	}
	return CreateOptions{
		Queues:     queues,
		TxQueueLen: 10000,
	}
}

// MultiQueueDevice is implemented by TUN devices that can be driven from more
// than one queue. Devices without it use Read/Write on queue 0 only.
type MultiQueueDevice interface {
	QueueCount() int
	ReadQueue(q int, bufs [][]byte, sizes []int, offset int) (int, error)
	WriteQueue(q int, bufs [][]byte, offset int) (int, error)
	TxQueueLen() int    // effective tx_queue_len read back from the kernel
	Backpressure() bool // true when IFF_BACKPRESSURE was accepted by the kernel
}

type Device interface {
	// File returns the file descriptor of the device.
	File() *os.File

	// Read one or more packets from the Device (without any additional headers).
	// On a successful read it returns the number of packets read, and sets
	// packet lengths within the sizes slice. len(sizes) must be >= len(bufs).
	// A nonzero offset can be used to instruct the Device on where to begin
	// reading into each element of the bufs slice.
	Read(bufs [][]byte, sizes []int, offset int) (n int, err error)

	// Write one or more packets to the device (without any additional headers).
	// On a successful write it returns the number of packets written. A nonzero
	// offset can be used to instruct the Device on where to begin writing from
	// each packet contained within the bufs slice.
	Write(bufs [][]byte, offset int) (int, error)

	// MTU returns the MTU of the Device.
	MTU() (int, error)

	// Name returns the current name of the Device.
	Name() (string, error)

	// Events returns a channel of type Event, which is fed Device events.
	Events() <-chan Event

	// Close stops the Device and closes the Event channel.
	Close() error

	// BatchSize returns the preferred/max number of packets that can be read or
	// written in a single read/write call. BatchSize must not change over the
	// lifetime of a Device.
	BatchSize() int
}
