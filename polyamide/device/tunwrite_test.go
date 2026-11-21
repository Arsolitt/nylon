package device

import (
	"encoding/binary"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/encodeous/nylon/perf"
	"github.com/encodeous/nylon/polyamide/conn"
	"github.com/encodeous/nylon/polyamide/tun"
)

// recordingTun is a single-queue tun.Device that records the frames it is
// handed, in delivery order, and how many frames each Write call carried.
type recordingTun struct {
	mu     sync.Mutex
	seqs   []uint32 // 4-byte payload of each delivered frame
	calls  []int    // frames per Write call
	events chan tun.Event

	// writeGate, when non-nil, blocks every Write call until it is closed. It
	// is set before the writer goroutines start, so it is read without a lock.
	writeGate <-chan struct{}
}

func newRecordingTun() *recordingTun { return &recordingTun{events: make(chan tun.Event, 1)} }

func (t *recordingTun) File() *os.File { return nil }

func (t *recordingTun) Read(_ [][]byte, _ []int, _ int) (int, error) {
	return 0, os.ErrClosed
}

func (t *recordingTun) Write(bufs [][]byte, offset int) (int, error) {
	if t.writeGate != nil {
		<-t.writeGate
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, len(bufs))
	for _, b := range bufs {
		t.seqs = append(t.seqs, binary.BigEndian.Uint32(b[offset:]))
	}
	return len(bufs), nil
}

func (t *recordingTun) MTU() (int, error)        { return DefaultMTU, nil }
func (t *recordingTun) Name() (string, error)    { return "rectun0", nil }
func (t *recordingTun) Events() <-chan tun.Event { return t.events }
func (t *recordingTun) Close() error             { return nil }
func (t *recordingTun) BatchSize() int           { return conn.IdealBatchSize }

func (t *recordingTun) maxFramesPerCall() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	max := 0
	for _, n := range t.calls {
		if n > max {
			max = n
		}
	}
	return max
}

func (t *recordingTun) frameCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seqs)
}

func (t *recordingTun) delivered() []uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]uint32(nil), t.seqs...)
}

// newBounceDevice builds a Device with one TUN writer queue and no writer
// goroutine. PopulatePools reads BatchSize, so the fake TUN is installed first.
func newBounceDevice(t *testing.T) (*Device, *recordingTun) {
	t.Helper()
	return newBounceDeviceQueues(t, 1)
}

// bounceElems allocates n bounce elements carrying the 4-byte sequence in
// [MessageTransportHeaderSize:] — the frame the writer hands to the TUN.
func bounceElems(dev *Device, first uint32, n int) []*TCElement {
	elems := make([]*TCElement, 0, n)
	for i := range n {
		e := dev.GetTCElement()
		e.Buffer = dev.GetMessageBuffer()
		e.Padding = 0
		e.Packet = e.Buffer[MessageTransportHeaderSize : MessageTransportHeaderSize+4]
		binary.BigEndian.PutUint32(e.Packet, first+uint32(i))
		elems = append(elems, e)
	}
	return elems
}

// TestTunWriterCoalescesQueuedBatches pins the property that makes the offload
// path useful: a backlog of one-frame batches must become one write call
// carrying a full frame vector, not one call per frame.
func TestTunWriterCoalescesQueuedBatches(t *testing.T) {
	dev, rec := newBounceDevice(t)
	tcs := NewTCState()

	// Fill the queue before the writer exists, so the first call is guaranteed
	// to observe the whole backlog.
	for i := range conn.IdealBatchSize {
		dev.enqueueBounceWrite(tcs, bounceElems(dev, uint32(i), 1))
	}

	dev.tun.wg.Add(1)
	go dev.RoutineTUNWriter(0, dev.tun.writers[0])
	defer func() {
		close(dev.tun.writers[0].stop)
		dev.tun.wg.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for rec.frameCount() < conn.IdealBatchSize && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rec.frameCount(); got != conn.IdealBatchSize {
		t.Fatalf("writer delivered %d/%d frames", got, conn.IdealBatchSize)
	}
	if got := rec.maxFramesPerCall(); got != conn.IdealBatchSize {
		t.Fatalf("largest write call carried %d frames, want %d (one call per queued batch)", got, conn.IdealBatchSize)
	}
	seqs := rec.delivered()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("delivery order broken at frame %d: %d after %d", i, seqs[i], seqs[i-1])
		}
	}
}

// TestBounceQueueFramesBounded floods a writer queue that no writer is draining
// and pins three invariants: the queue is bounded in frames, every dropped
// frame is counted, and the frames that survive are an in-order prefix (the
// newest whole batch is refused, so no reordering is introduced).
func TestBounceQueueFramesBounded(t *testing.T) {
	dev, _ := newBounceDevice(t)
	tcs := NewTCState()

	const perBatch = 64
	// Offer 16x the frame budget so the admission policy is actually
	// exercised: a budget-sized offer would fit without a single drop.
	batches := 16 * QueueWriteFrames / perBatch
	before := perf.TunWriteQueueDropsTotal.Load()
	next := uint32(0)
	for range batches {
		dev.enqueueBounceWrite(tcs, bounceElems(dev, next, perBatch))
		next += perBatch
	}
	dropped := perf.TunWriteQueueDropsTotal.Load() - before

	var delivered []uint32
	for {
		select {
		case batch := <-dev.tun.writers[0].c:
			for _, frame := range batch {
				delivered = append(delivered, binary.BigEndian.Uint32(frame.buf[frame.offset:]))
			}
		default:
			goto drained
		}
	}
drained:
	if want := uint64(batches*perBatch) - uint64(len(delivered)); dropped != want {
		t.Fatalf("dropped %d frames, want %d (%d accepted of %d offered)", dropped, want, len(delivered), batches*perBatch)
	}
	if len(delivered) > QueueWriteFrames {
		t.Fatalf("queue held %d frames, want at most %d", len(delivered), QueueWriteFrames)
	}
	if len(delivered) == 0 {
		t.Fatal("queue accepted nothing")
	}
	for i := 1; i < len(delivered); i++ {
		if delivered[i] != delivered[i-1]+1 {
			t.Fatalf("delivery order broken at frame %d: %d after %d", i, delivered[i], delivered[i-1])
		}
	}
	if last := delivered[len(delivered)-1]; last != uint32(len(delivered)-1) {
		t.Fatalf("last accepted frame is %d, want %d: the dropped frames must be the newest", last, len(delivered)-1)
	}
	t.Logf("accepted %d frames of %d, dropped %d", len(delivered), batches*perBatch, dropped)
}

// TestBounceQueueFramesRightSized pins that a queued bounce frame is a
// right-sized copy of its packet, not a reference to a full 64 KiB message
// buffer: the queue's memory is its frame count times its frame size.
func TestBounceQueueFramesRightSized(t *testing.T) {
	dev, _ := newBounceDevice(t)
	tcs := NewTCState()

	const (
		payload = 4
		seq     = 7
	)
	dev.enqueueBounceWrite(tcs, bounceElems(dev, seq, 1))

	var frames []tunWriteFrame
	select {
	case frames = <-dev.tun.writers[0].c:
	default:
		t.Fatal("no batch was queued")
	}
	if len(frames) != 1 {
		t.Fatalf("queued %d frames, want 1", len(frames))
	}
	frame := frames[0]
	if got, want := len(frame.buf), frame.offset+payload; got != want {
		t.Fatalf("queued frame buffer is %d bytes, want %d (offset %d + %d-byte payload)",
			got, want, frame.offset, payload)
	}
	if limit := MaxMessageSize / 4; len(frame.buf) >= limit {
		t.Fatalf("queued frame pins %d bytes, want less than %d", len(frame.buf), limit)
	}
	if got := binary.BigEndian.Uint32(frame.buf[frame.offset:]); got != seq {
		t.Fatalf("queued frame carries sequence %d, want %d", got, seq)
	}
}

// newBounceDeviceQueues builds a Device with queues TUN writer queues and no
// writer goroutines. PopulatePools reads BatchSize, so the fake TUN is
// installed first.
func newBounceDeviceQueues(t *testing.T, queues int) (*Device, *recordingTun) {
	t.Helper()
	rec := newRecordingTun()
	dev := &Device{}
	dev.Log = NewLogger(LogLevelError, "test: ")
	dev.net.bind = &fakeBindSized{size: conn.IdealBatchSize}
	dev.tun.device = rec
	dev.tun.queues = queues
	dev.tun.writers = make([]*tunWriteQueue, queues)
	for i := range dev.tun.writers {
		dev.tun.writers[i] = newTunWriteQueue()
	}
	dev.PopulatePools()
	return dev, rec
}

// bounceElemsPadded allocates n bounce elements for one source peer, cycling
// the given paddings. The 4-byte sequence is written at the frame offset the
// writer will hand to the TUN, so it identifies the frame after coalescing
// splits the batch by padding.
func bounceElemsPadded(dev *Device, first uint32, n int, paddings []uint32, from *Peer) []*TCElement {
	elems := make([]*TCElement, 0, n)
	for i := range n {
		padding := paddings[i%len(paddings)]
		offset := int(padding) + MessageTransportHeaderSize
		e := dev.GetTCElement()
		e.Buffer = dev.GetMessageBuffer()
		e.Padding = padding
		e.Packet = e.Buffer[offset : offset+4]
		binary.BigEndian.PutUint32(e.Packet, first+uint32(i))
		e.FromPeer = from
		elems = append(elems, e)
	}
	return elems
}

func waitQueueDrained(t *testing.T, wq *tunWriteQueue) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for wq.queued.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("writer queue did not drain, %d frames still queued", wq.queued.Load())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestBounceWriteMultisetPreserved streams more batches than a writer queue
// holds, with mixed paddings and two queue indices, while the writers run. It
// pins the invariant that every enqueued frame reaches the TUN exactly once:
// the delivered frame multiset equals the enqueued one, so a lost frame covered
// by a duplicate cannot pass, and the queue admission policy drops nothing.
func TestBounceWriteMultisetPreserved(t *testing.T) {
	const queues = 2
	dev, rec := newBounceDeviceQueues(t, queues)
	tcs := NewTCState()

	peers := make([]*Peer, queues)
	for i := range peers {
		peers[i] = &Peer{writeIndex: i}
	}

	for q := range queues {
		dev.tun.wg.Add(1)
		go dev.RoutineTUNWriter(q, dev.tun.writers[q])
	}
	defer func() {
		for _, wq := range dev.tun.writers {
			close(wq.stop)
		}
		dev.tun.wg.Wait()
	}()

	const perBatch = 3
	paddings := []uint32{0, 3, 0}
	batches := QueueWriteFrames + QueueWriteFrames/4
	want := make([]uint32, 0, batches*perBatch)
	dropsBefore := perf.TunWriteQueueDropsTotal.Load()
	next := uint32(0)
	for i := range batches {
		q := i % queues
		// Drain the queue before the next handoff so admission never drops:
		// this test is about duplication, not the drop policy.
		waitQueueDrained(t, dev.tun.writers[q])
		dev.enqueueBounceWrite(tcs, bounceElemsPadded(dev, next, perBatch, paddings, peers[q]))
		for j := range perBatch {
			want = append(want, next+uint32(j))
		}
		next += perBatch
	}

	deadline := time.Now().Add(10 * time.Second)
	for rec.frameCount() < len(want) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := rec.frameCount(); got != len(want) {
		t.Fatalf("writer delivered %d/%d frames", got, len(want))
	}
	if drops := perf.TunWriteQueueDropsTotal.Load() - dropsBefore; drops != 0 {
		t.Fatalf("queue dropped %d frames while the writers kept up", drops)
	}

	got := rec.delivered()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("delivered frame multiset differs from enqueued: got %d frames, want %d", len(got), len(want))
	}
}

// TestBounceBurstNoDuplicates floods two writer queues past their frame budget
// without draining, so the writers run their batch-drain loop and the drops
// that the admission policy counts happen while the writers are mid-batch. It
// pins two invariants that a paused-then-drained producer cannot reach: no
// frame is delivered twice, and every enqueued frame is either delivered or
// counted as a drop.
func TestBounceBurstNoDuplicates(t *testing.T) {
	const queues = 2
	dev, rec := newBounceDeviceQueues(t, queues)
	tcs := NewTCState()

	// A real TUN write is a syscall, so the writer cannot keep up with the
	// producer. Hold the writers inside their first write call, flood past the
	// frame budget so the drop path really runs, then release them. Without
	// the hold, the fake writer outruns the producer, the queue never fills
	// and no drop is exercised at all.
	gate := make(chan struct{})
	rec.writeGate = gate
	release := sync.OnceFunc(func() { close(gate) })
	defer release()

	peers := make([]*Peer, queues)
	for i := range peers {
		peers[i] = &Peer{writeIndex: i}
	}
	for q := range queues {
		dev.tun.wg.Add(1)
		go dev.RoutineTUNWriter(q, dev.tun.writers[q])
	}
	defer func() {
		for _, wq := range dev.tun.writers {
			close(wq.stop)
		}
		dev.tun.wg.Wait()
	}()

	const (
		perBatch = 3
		batches  = 4 * QueueWriteFrames
	)
	paddings := []uint32{0, 3}
	enqueued := make(map[uint32]struct{}, batches*perBatch)
	dropsBefore := perf.TunWriteQueueDropsTotal.Load()
	next := uint32(0)
	for i := range batches {
		q := i % queues
		dev.enqueueBounceWrite(tcs, bounceElemsPadded(dev, next, perBatch, paddings, peers[q]))
		for j := range perBatch {
			enqueued[next+uint32(j)] = struct{}{}
		}
		next += perBatch
	}
	release()
	total := uint64(batches * perBatch)

	deadline := time.Now().Add(10 * time.Second)
	for {
		drops := perf.TunWriteQueueDropsTotal.Load() - dropsBefore
		if uint64(rec.frameCount())+drops == total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d + dropped %d of %d frames after 10s",
				rec.frameCount(), drops, total)
		}
		time.Sleep(time.Millisecond)
	}

	seen := make(map[uint32]int, len(enqueued))
	delivered := rec.delivered()
	t.Logf("delivered %d frames, dropped %d of %d", len(delivered),
		perf.TunWriteQueueDropsTotal.Load()-dropsBefore, total)
	for _, seq := range delivered {
		if _, ok := enqueued[seq]; !ok {
			t.Fatalf("delivered frame %d was never enqueued", seq)
		}
		seen[seq]++
		if seen[seq] > 1 {
			t.Fatalf("frame %d delivered %d times", seq, seen[seq])
		}
	}
}
