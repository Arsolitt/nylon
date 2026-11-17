package device

import (
	"encoding/binary"
	"os"
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
}

func newRecordingTun() *recordingTun { return &recordingTun{events: make(chan tun.Event, 1)} }

func (t *recordingTun) File() *os.File { return nil }

func (t *recordingTun) Read(_ [][]byte, _ []int, _ int) (int, error) {
	return 0, os.ErrClosed
}

func (t *recordingTun) Write(bufs [][]byte, offset int) (int, error) {
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
	rec := newRecordingTun()
	dev := &Device{}
	dev.Log = NewLogger(LogLevelError, "test: ")
	dev.net.bind = &fakeBindSized{size: conn.IdealBatchSize}
	dev.tun.device = rec
	dev.tun.queues = 1
	dev.tun.writers = []*tunWriteQueue{newTunWriteQueue()}
	dev.PopulatePools()
	return dev, rec
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
	const batches = 64 // 4096 frames offered, 16x the frame budget
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
			for _, e := range batch {
				delivered = append(delivered, binary.BigEndian.Uint32(e.Packet))
				dev.PutMessageBuffer(e.Buffer)
				dev.PutTCElement(e)
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
