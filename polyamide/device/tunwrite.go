/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"errors"
	"os"
	"sync/atomic"

	"github.com/encodeous/nylon/perf"
	"github.com/encodeous/nylon/polyamide/conn"
)

// maxTunWriteBatch is the largest frame vector a single TUN write call carries.
const maxTunWriteBatch = conn.IdealBatchSize

// init publishes the per-platform frame budget, so the drop counters can be
// read against their limit on the metrics endpoint.
func init() {
	perf.TunWriteQueueCapacityFrames.Store(uint64(QueueWriteFrames))
}

// tunWriteFrame is one bounce frame copied into a right-sized buffer, so a
// queued frame costs its own length instead of a full 64 KiB message buffer.
type tunWriteFrame struct {
	buf    []byte // [padding][transport header][packet], exactly what the TUN write consumes
	offset int    // first packet byte inside buf
}

// tunWriteQueue hands bounce frames to a single TUN writer goroutine. A batch
// is drained by exactly one writer, which preserves the order of the frames
// handed to it.
type tunWriteQueue struct {
	c    chan []tunWriteFrame
	stop chan struct{}
	// queued counts frames accepted into c and not yet written. The channel
	// caps accepted groups, but a group can carry any number of frames, so the
	// buffer is budgeted in frames: the bound must not depend on the producer
	// batch size.
	queued atomic.Int64
}

func newTunWriteQueue() *tunWriteQueue {
	// The channel capacity bounds accepted groups; queued is the frame bound,
	// and it is the one that normally binds first because a group carries
	// several frames.
	return &tunWriteQueue{
		c:    make(chan []tunWriteFrame, QueueWriteFrames),
		stop: make(chan struct{}),
	}
}

// enqueueBounceWrite hands bounce elements to the per-queue writers, preserving
// per-peer order. Each element is copied into a right-sized frame and recycled
// before the handoff, so a queued frame never pins a message buffer. Frames
// dropped because a writer queue is full are counted.
func (device *Device) enqueueBounceWrite(tcs *TCState, elems []*TCElement) {
	if len(elems) == 0 {
		return
	}
	// The device is closing: the writers are gone or about to be, and the TUN
	// itself is already closed, so nothing can reach the system any more.
	if device.isClosed() {
		device.recycleBounce(elems)
		return
	}
	if len(tcs.bounceGroups) < device.tun.queues {
		tcs.bounceGroups = make([][]*TCElement, device.tun.queues)
	}
	groups := tcs.bounceGroups
	for _, elem := range elems {
		idx := 0
		if elem.FromPeer != nil {
			idx = elem.FromPeer.writeIndex
		}
		if idx >= len(groups) {
			idx = 0
		}
		groups[idx] = append(groups[idx], elem)
	}
	for idx, group := range groups {
		if len(group) == 0 {
			continue
		}
		wq := device.tun.writers[idx]
		if wq == nil {
			device.recycleBounce(group)
			groups[idx] = group[:0]
			continue
		}
		n := int64(len(group))
		if wq.queued.Add(n) > int64(QueueWriteFrames) {
			wq.queued.Add(-n)
			perf.TunWriteQueueDropsTotal.Add(uint64(len(group)))
			device.recycleBounce(group)
			groups[idx] = group[:0]
			continue
		}
		// The frame slice is private to the writer, so the pooled message
		// buffers go back before the handoff.
		frames := make([]tunWriteFrame, 0, len(group))
		for _, elem := range group {
			offset := int(elem.Padding) + MessageTransportHeaderSize
			size := offset + len(elem.Packet)
			buf := make([]byte, size)
			copy(buf, elem.Buffer[:size])
			frames = append(frames, tunWriteFrame{buf: buf, offset: offset})
		}
		device.recycleBounce(group)
		groups[idx] = group[:0]
		select {
		case wq.c <- frames: // ownership moves to the writer
		case <-wq.stop:
			wq.queued.Add(-n)
		default:
			wq.queued.Add(-n)
			perf.TunWriteQueueDropsTotal.Add(uint64(len(frames)))
		}
	}
}

// RoutineTUNWriter drains one TUN writer queue. Bouncing packets back into the
// system is a syscall per frame, so it is done off the receive goroutines.
func (device *Device) RoutineTUNWriter(q int, wq *tunWriteQueue) {
	defer func() {
		device.Log.Verbosef("Routine: TUN writer %d - stopped", q)
		device.tun.wg.Done()
	}()

	device.Log.Verbosef("Routine: TUN writer %d - started", q)

	elems := make([]tunWriteFrame, 0, maxTunWriteBatch)
	bufs := make([][]byte, 0, maxTunWriteBatch)
	for {
		select {
		case batch := <-wq.c:
			elems = append(elems[:0], batch...)
			// A producer hands over about one frame at a time, so take every
			// batch that is already queued: one write call then carries a full
			// flow run, which is what lets the offload path coalesce frames
			// into GSO packets instead of one syscall per frame.
		drain:
			for len(elems) < maxTunWriteBatch {
				select {
				case more := <-wq.c:
					elems = append(elems, more...)
				default:
					break drain
				}
			}
			bufs = device.writeBounceBatch(q, elems, bufs)
			wq.queued.Add(-int64(len(elems)))
			clear(elems)
			elems = elems[:0]
		case <-wq.stop:
			return
		}
	}
}

// writeBounceBatch writes batch to the TUN, coalescing consecutive frames that
// share a frame offset into one call, and reuses the bufs scratch. A TUN write
// is one frame per syscall and writev would merge the frames into a single
// bogus packet, so batching is only possible through the offload path: with
// IFF_VNET_HDR the Linux writer runs userspace GRO over the vector and emits
// one GSO frame per coalesced flow run. That pays off only when a call carries
// several frames of one flow, which is why the caller drains queued batches
// before writing.
func (device *Device) writeBounceBatch(q int, batch []tunWriteFrame, bufs [][]byte) [][]byte {
	for len(batch) > 0 {
		offset := batch[0].offset
		// here, we need to pass the full buf instead of buf[offset:] since the
		// TUN writer applies offset again and we will get io.ErrShortBuffer
		n := 0
		for n < len(batch) && batch[n].offset == offset {
			bufs = append(bufs, batch[n].buf)
			n++
		}

		var err error
		if device.tun.multi != nil {
			_, err = device.tun.multi.WriteQueue(q, bufs, offset)
		} else {
			_, err = device.tun.device.Write(bufs, offset)
		}
		perf.TunWriteBatchSize.Add(float64(len(bufs)))
		if err != nil {
			if !errors.Is(err, os.ErrClosed) && !device.isClosed() {
				device.Log.Errorf("Failed to loop back packets to TUN device: %v", err)
			}
			return bufs[:0]
		}

		batch = batch[n:]
		bufs = bufs[:0]
	}
	return bufs
}

// recycleBounce returns a batch's buffers and elements to the device pools.
// Frames accepted by a writer queue are right-sized copies instead.
func (device *Device) recycleBounce(batch []*TCElement) {
	for _, elem := range batch {
		if elem == nil {
			continue
		}
		device.PutMessageBuffer(elem.Buffer)
		device.PutTCElement(elem)
	}
}

// drainWriteQueue releases the frame accounting for a stopped writer. The
// queued frames are copies and are left to the garbage collector.
func (device *Device) drainWriteQueue(wq *tunWriteQueue) {
	for {
		select {
		case batch := <-wq.c:
			wq.queued.Add(-int64(len(batch)))
		default:
			return
		}
	}
}

// closeTunWriters stops every TUN writer and reclaims the batches they never
// picked up. The stop channels are never closed twice: Close is idempotent.
func (device *Device) closeTunWriters() {
	for _, wq := range device.tun.writers {
		if wq != nil {
			close(wq.stop)
		}
	}
	device.tun.wg.Wait()
	for _, wq := range device.tun.writers {
		if wq != nil {
			device.drainWriteQueue(wq)
		}
	}
}
