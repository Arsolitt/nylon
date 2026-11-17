/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import "github.com/encodeous/nylon/polyamide/conn"

const (
	QueueStagedSize            = 128
	QueueOutboundSize          = 1024
	QueueInboundSize           = 1024
	QueueHandshakeSize         = 1024
	QueueWriteFrames           = 2 * conn.IdealBatchSize // frames buffered per TUN writer queue before dropping
	MaxSegmentSize             = 2048 - 32               // largest possible UDP datagram
	PreallocatedBuffersPerPool = 0                       // Disable and allow for infinite memory growth
)
