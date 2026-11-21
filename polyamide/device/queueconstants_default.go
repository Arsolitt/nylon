//go:build !android && !ios && !windows

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import "github.com/encodeous/nylon/polyamide/conn"

const (
	QueueStagedSize    = conn.IdealBatchSize
	QueueOutboundSize  = 1024
	QueueInboundSize   = 1024
	QueueHandshakeSize = 1024
	// 4096 frames is roughly 300 ms at the measured 13.4k frames/s ceiling and
	// >=38x the largest measured burst (107 frames). Raise it only together
	// with the frame size, because the queue's memory is frames x
	// bytes-per-frame.
	QueueWriteFrames           = 4096
	MaxSegmentSize             = (1 << 16) - 1 // largest possible UDP datagram
	PreallocatedBuffersPerPool = 0             // Disable and allow for infinite memory growth
)
