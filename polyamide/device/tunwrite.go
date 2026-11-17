/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"errors"
	"os"
	"slices"
	"sync/atomic"

	"github.com/encodeous/nylon/perf"
	"github.com/encodeous/nylon/polyamide/conn"
)

// maxTunWriteBatch is the largest frame vector a single TUN write call carries.
const maxTunWriteBatch = conn.IdealBatchSize

// tunWriteQueue hands bounce batches to a single TUN writer goroutine. A batch
// is drained by exactly one writer, which preserves the order of the elements
// handed to it.
type tunWriteQueue struct {
	c    chan []*TCElement
	stop chan struct{}
	// queued counts frames accepted into c and not yet written. The channel
	// caps batches, but a batch holds up to one ideal batch of frames, so the
	// buffer is budgeted in frames to keep the bound independent of the
	// producer batch size.
	queued atomic.Int64
}

func newTunWriteQueue() *tunWriteQueue {
	// The channel capacity bounds batches; queued is the frame bound, and it is
	// the one that normally binds first because c is never filled with full
	// ideal-size batches in practice.
	return &tunWriteQueue{
		c:    make(chan []*TCElement, QueueWriteFrames),
		stop: make(chan struct{}),
	}
}

// enqueueBounceWrite hands bounce elements to the per-queue writers, preserving
// per-peer order. Elements dropped because a writer queue is full are recycled
// immediately and counted.
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
		// The caller reuses its scratch, so the batch is a copy.
		batch := slices.Clone(group)
		groups[idx] = group[:0]
		wq := device.tun.writers[idx]
		if wq == nil {
			device.recycleBounce(batch)
			continue
		}
		n := int64(len(batch))
		if wq.queued.Add(n) > int64(QueueWriteFrames) {
			wq.queued.Add(-n)
			perf.TunWriteQueueDropsTotal.Add(uint64(len(batch)))
			device.recycleBounce(batch)
			continue
		}
		select {
		case wq.c <- batch: // ownership moves to the writer
		case <-wq.stop:
			wq.queued.Add(-n)
			device.recycleBounce(batch)
		default:
			wq.queued.Add(-n)
			perf.TunWriteQueueDropsTotal.Add(uint64(len(batch)))
			device.recycleBounce(batch)
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

	elems := make([]*TCElement, 0, maxTunWriteBatch)
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
			device.recycleBounce(elems)
			clear(elems)
			elems = elems[:0]
		case <-wq.stop:
			return
		}
	}
}

// writeBounceBatch writes batch to the TUN, coalescing consecutive elements
// that share a frame offset into one call, and reuses the bufs scratch. A TUN
// write is one frame per syscall and writev would merge the frames into a
// single bogus packet, so batching is only possible through the offload path:
// with IFF_VNET_HDR the Linux writer runs userspace GRO over the vector and
// emits one GSO frame per coalesced flow run. That pays off only when a call
// carries several frames of one flow, which is why the caller drains queued
// batches before writing.
func (device *Device) writeBounceBatch(q int, batch []*TCElement, bufs [][]byte) [][]byte {
	for len(batch) > 0 {
		offset := int(batch[0].Padding) + MessageTransportHeaderSize
		// here, we need to use elem.Buffer instead of elem.Packet since we will get io.ErrShortBuffer if offset < 4
		n := 0
		for n < len(batch) && int(batch[n].Padding)+MessageTransportHeaderSize == offset {
			bufs = append(bufs, batch[n].Buffer[:offset+len(batch[n].Packet)])
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
func (device *Device) recycleBounce(batch []*TCElement) {
	for _, elem := range batch {
		if elem == nil {
			continue
		}
		device.PutMessageBuffer(elem.Buffer)
		device.PutTCElement(elem)
	}
}

// drainWriteQueue recycles everything still queued for a stopped writer.
func (device *Device) drainWriteQueue(wq *tunWriteQueue) {
	for {
		select {
		case batch := <-wq.c:
			wq.queued.Add(-int64(len(batch)))
			device.recycleBounce(batch)
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
