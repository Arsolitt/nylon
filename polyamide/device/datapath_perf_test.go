package device

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/encodeous/nylon/perf"
	"github.com/encodeous/nylon/polyamide/conn"
	"github.com/encodeous/nylon/polyamide/conn/bindtest"
	"github.com/encodeous/nylon/polyamide/tun"
	"github.com/encodeous/nylon/polyamide/tun/tuntest"
	"golang.org/x/crypto/chacha20poly1305"
)

// The tests in this file exercise the TUN drain path itself. They use a fake TUN
// that never blocks on reads, so a reader parked in a downstream handoff shows
// up as a plateau in its read counter instead of as a silent stall.

const (
	holPollInterval = 100 * time.Microsecond // holTun read poll when nothing is queued
	holChannelSize  = 128                    // inbound/outbound channel depth, as tuntest.ChannelTUN

	holFloodPackets    = 12000 // packets addressed to the stalled peer before giving up
	holFloodPace       = 200 * time.Microsecond
	holReadPoll        = 25 * time.Millisecond
	holStallWindow     = 300 * time.Millisecond // no read calls for this long means the reader is parked
	holStallCap        = 5 * time.Second
	holHealthyPackets  = 100
	holHealthyDeadline = 2 * time.Second
	holResumeTimeout   = 2 * time.Second
	holReceivePoll     = 10 * time.Millisecond

	benchStallTimeout = 5 * time.Second // no drain progress at all means the pipeline is wedged
	benchStallPoll    = 250 * time.Millisecond
	benchDrainPoll    = time.Millisecond
)

// holTun is a multi-queue fake TUN with the same channel semantics as
// tuntest.ChannelTUN. Reads never block: a read that finds nothing sleeps for
// holPollInterval and reports zero packets, so a reader that is parked
// downstream stops accumulating read calls and becomes observable.
type holTun struct {
	Inbound  chan []byte // packets delivered to the system, closed on TUN close
	Outbound chan []byte // packets handed to the device by the system

	queues    int
	readCalls atomic.Uint64
	written   atomic.Uint64 // packets delivered to the system through the TUN write path
	closed    chan struct{}
	events    chan tun.Event
	closeOnce sync.Once
}

func newHOLTun(queues int) *holTun {
	t := &holTun{
		Inbound:  make(chan []byte, holChannelSize),
		Outbound: make(chan []byte, holChannelSize),
		queues:   queues,
		closed:   make(chan struct{}),
		events:   make(chan tun.Event, 1),
	}
	t.events <- tun.EventUp
	return t
}

func (t *holTun) ReadQueue(q int, bufs [][]byte, sizes []int, offset int) (int, error) {
	t.readCalls.Add(1)
	select {
	case <-t.closed:
		return 0, os.ErrClosed
	default:
	}
	n := 0
drain:
	for n < len(bufs) {
		select {
		case msg := <-t.Outbound:
			copy(bufs[n][offset:], msg)
			sizes[n] = len(msg)
			n++
		default:
			break drain
		}
	}
	if n == 0 {
		time.Sleep(holPollInterval)
	}
	return n, nil
}

func (t *holTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	return t.ReadQueue(0, bufs, sizes, offset)
}

func (t *holTun) WriteQueue(q int, bufs [][]byte, offset int) (int, error) {
	for i, buf := range bufs {
		msg := make([]byte, len(buf)-offset)
		copy(msg, buf[offset:])
		select {
		case <-t.closed:
			return i, os.ErrClosed
		case t.Inbound <- msg:
			t.written.Add(1)
		}
	}
	return len(bufs), nil
}

func (t *holTun) Write(bufs [][]byte, offset int) (int, error) {
	return t.WriteQueue(0, bufs, offset)
}

func (t *holTun) QueueCount() int       { return t.queues }
func (t *holTun) TxQueueLen() int       { return 0 }
func (t *holTun) Backpressure() bool    { return false }
func (t *holTun) BatchSize() int        { return conn.IdealBatchSize }
func (t *holTun) MTU() (int, error)     { return DefaultMTU, nil }
func (t *holTun) Name() (string, error) { return "holtest0", nil }
func (t *holTun) File() *os.File        { return nil }
func (t *holTun) Events() <-chan tun.Event {
	return t.events
}

func (t *holTun) Close() error {
	t.closeOnce.Do(func() {
		close(t.closed)
		close(t.events)
	})
	return nil
}

// stallingBind wraps a conn.Bind and, once armed, blocks every send to the
// stalled peer's endpoint, modelling a peer whose transmission path has
// stalled. Releasing the stall drops those sends instead of forwarding them, so
// that teardown never blocks on the stalled peer.
type stallingBind struct {
	inner     conn.Bind
	block     chan struct{}
	blockDst  atomic.Pointer[string]
	armed     atomic.Bool
	releaseOn sync.Once
}

func newStallingBind(inner conn.Bind) *stallingBind {
	return &stallingBind{
		inner: inner,
		block: make(chan struct{}),
	}
}

// stall configures the endpoint whose sends are blocked. It must be called
// before arm.
func (b *stallingBind) stall(dst string) { b.blockDst.Store(&dst) }

func (b *stallingBind) arm() { b.armed.Store(true) }

func (b *stallingBind) release() {
	b.releaseOn.Do(func() {
		close(b.block)
	})
}

func (b *stallingBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	if b.armed.Load() {
		if dst := b.blockDst.Load(); dst != nil && ep.DstToString() == *dst {
			<-b.block
			return nil // the stall was released: drop, never forward
		}
	}
	return b.inner.Send(bufs, ep)
}

func (b *stallingBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	return b.inner.Open(port)
}

func (b *stallingBind) Close() error              { return b.inner.Close() }
func (b *stallingBind) SetMark(mark uint32) error { return b.inner.SetMark(mark) }
func (b *stallingBind) BatchSize() int            { return b.inner.BatchSize() }
func (b *stallingBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	return b.inner.ParseEndpoint(s)
}

// sinkBind is a conn.Bind that accepts and discards every send, counting the
// packets, and never receives. It lets a benchmark measure the outbound TUN
// pipeline up to the wire handoff without a peer that has to keep up.
type sinkBind struct {
	batchSize int
	packets   atomic.Uint64
}

var _ conn.Bind = (*sinkBind)(nil)

func (b *sinkBind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) { return nil, 1, nil }
func (b *sinkBind) Close() error                                         { return nil }
func (b *sinkBind) SetMark(mark uint32) error                            { return nil }
func (b *sinkBind) BatchSize() int                                       { return b.batchSize }

func (b *sinkBind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.packets.Add(uint64(len(bufs)))
	return nil
}

func (b *sinkBind) ParseEndpoint(s string) (conn.Endpoint, error) {
	return bindtest.ChannelEndpoint(1), nil
}

// holPeer is a testPeer whose TUN is a holTun.
type holPeer struct {
	tun *holTun
	dev *Device
	ip  netip.Addr
}

// holTestPair is a three-device topology. pair[1] is the device under test: it
// has two peers, pair[0] and pair[2], and its TUN is the one being drained.
// pair[2] is the peer whose transmission path can be stalled.
//
// The devices talk to each other over real loopback sockets: bindtest's channel
// fabric only connects pairs, and this test needs one device with two peers. A
// stallingBind is installed in front of the device under test's bind so that
// sends to a single peer can be parked.
type holTestPair [3]holPeer

// exchange pushes one ping from device from to device to and waits for it to
// arrive, which also establishes the keypair for that pair in both directions.
func (pair *holTestPair) exchange(tb testing.TB, from, to int) {
	tb.Helper()
	msg := tuntest.Ping(pair[to].ip, pair[from].ip)
	select {
	case pair[from].tun.Outbound <- msg:
	case <-time.After(5 * time.Second):
		tb.Fatalf("device %d could not hand a ping to device %d", from, to)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case got := <-pair[to].tun.Inbound:
		if !bytes.Equal(msg, got) {
			tb.Fatalf("ping from device %d to device %d did not transit correctly", from, to)
		}
	case <-timer.C:
		tb.Fatalf("ping from device %d did not reach device %d", from, to)
	}
}

// burst hands n copies of pkt from device from to device to and waits for all
// of them to arrive.
func (pair *holTestPair) burst(tb testing.TB, from, to int, pkt []byte, n int) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for i := range n {
		select {
		case pair[from].tun.Outbound <- pkt:
		case <-time.After(time.Until(deadline)):
			tb.Fatalf("device %d could not hand packet %d/%d to the TUN", from, i+1, n)
		}
	}
	for i := range n {
		select {
		case got := <-pair[to].tun.Inbound:
			if !bytes.Equal(pkt, got) {
				tb.Fatalf("packet %d/%d from device %d to device %d did not transit correctly", i+1, n, from, to)
			}
		case <-time.After(time.Until(deadline)):
			tb.Fatalf("device %d received %d/%d packets from device %d", to, i, n, from)
		}
	}
}

// newHOLTestPair builds the three-device topology with holTun devices. queues is
// the queue count reported by each fake TUN. The returned bind is installed in
// front of device 1's bind and blocks sends to device 2's endpoint once armed.
func newHOLTestPair(tb testing.TB, queues int) (pair holTestPair, stall *stallingBind) {
	tb.Helper()

	var keys [3]NoisePrivateKey
	var pubs [3]NoisePublicKey
	for i := range keys {
		if _, err := rand.Read(keys[i][:]); err != nil {
			tb.Fatalf("unable to generate private key random bytes: %v", err)
		}
		pubs[i] = keys[i].publicKey()
	}
	peerCfg := func(idx, allowed int) []string {
		return []string{
			"public_key", hex.EncodeToString(pubs[idx][:]),
			"protocol_version", "1",
			"replace_allowed_ips", "true",
			"allowed_ip", fmt.Sprintf("1.0.0.%d/32", allowed),
		}
	}
	cfgs := [3]string{
		uapiCfg(append([]string{
			"private_key", hex.EncodeToString(keys[0][:]),
			"listen_port", "0",
			"replace_peers", "true",
		}, peerCfg(1, 2)...)...),
		uapiCfg(append(append([]string{
			"private_key", hex.EncodeToString(keys[1][:]),
			"listen_port", "0",
			"replace_peers", "true",
		}, peerCfg(0, 1)...), peerCfg(2, 3)...)...),
		uapiCfg(append([]string{
			"private_key", hex.EncodeToString(keys[2][:]),
			"listen_port", "0",
			"replace_peers", "true",
		}, peerCfg(1, 2)...)...),
	}

	level := LogLevelVerbose
	if _, ok := tb.(*testing.B); ok && !testing.Verbose() {
		level = LogLevelError
	}
	for i := range pair {
		p := &pair[i]
		p.tun = newHOLTun(queues)
		p.ip = netip.AddrFrom4([4]byte{1, 0, 0, byte(i + 1)})
		bind := conn.NewDefaultBind()
		if i == 1 {
			// The stalling bind is handed to the constructor: the device must
			// never observe a bind being swapped in, and Device.BatchSize()
			// reads net.bind without holding the lock.
			stall = newStallingBind(bind)
			bind = stall
		}
		p.dev = NewDevice(p.tun, bind, NewLogger(level, fmt.Sprintf("dev%d: ", i)))
		if err := p.dev.IpcSet(cfgs[i]); err != nil {
			tb.Fatalf("failed to configure device %d: %v", i, err)
		}
		if err := p.dev.Up(); err != nil {
			tb.Fatalf("failed to bring up device %d: %v", i, err)
		}
	}

	var ports [3]uint16
	for i := range pair {
		ports[i] = pair[i].dev.net.port
	}
	endpoint := func(idx int) []string {
		return []string{"public_key", hex.EncodeToString(pubs[idx][:]), "endpoint", fmt.Sprintf("127.0.0.1:%d", ports[idx])}
	}
	endpointCfgs := [3]string{
		uapiCfg(endpoint(1)...),
		uapiCfg(append(endpoint(0), endpoint(2)...)...),
		uapiCfg(endpoint(1)...),
	}
	for i := range pair {
		p := &pair[i]
		if err := p.dev.IpcSet(endpointCfgs[i]); err != nil {
			tb.Fatalf("failed to configure device %d endpoints: %v", i, err)
		}
		// The device is ready. Close it when the test completes.
		tb.Cleanup(p.dev.Close)
	}

	stalled := pair[1].dev.LookupPeer(pubs[2])
	if stalled == nil {
		tb.Fatal("device 1 has no peer for device 2")
	}
	endpoints := stalled.GetEndpoints()
	if len(endpoints) == 0 {
		tb.Fatal("device 1's peer for device 2 has no endpoint")
	}
	stall.stall(endpoints[0].DstToString())
	return pair, stall
}

// floodStalledPeer hands packets addressed to the stalled peer to the device
// under test until stop is closed or the packet budget is exhausted. The flood
// is paced so the reader produces roughly one staged container per read.
func floodStalledPeer(stop <-chan struct{}, tunDev *holTun, pkt []byte) {
	ticker := time.NewTicker(holFloodPace)
	defer ticker.Stop()
	for range holFloodPackets {
		select {
		case tunDev.Outbound <- pkt:
		case <-stop:
			return
		}
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

// waitForReaderStall reports whether the TUN reader stopped issuing reads for
// window, i.e. it is parked in a downstream handoff. The wait ends early when
// the flood has drained and the reader is still reading: a reader that makes
// progress is by definition not parked, and waiting out the cap would only slow
// the fixed path down.
func waitForReaderStall(tunDev *holTun, floodFinished <-chan struct{}, window, limit time.Duration) bool {
	var (
		last       = tunDev.readCalls.Load()
		lastChange = time.Now()
		floodEnd   time.Time
		deadline   = time.Now().Add(limit)
	)
	for time.Now().Before(deadline) {
		time.Sleep(holReadPoll)
		now := time.Now()
		if calls := tunDev.readCalls.Load(); calls != last {
			last = calls
			lastChange = now
		} else if now.Sub(lastChange) >= window {
			return true
		}
		if floodEnd.IsZero() {
			select {
			case <-floodFinished:
				floodEnd = now
			default:
			}
		}
		if !floodEnd.IsZero() && now.Sub(floodEnd) >= window {
			return false
		}
	}
	return false
}

// waitForReaderResume reports whether the TUN reader issued another read within
// limit, i.e. it is no longer parked.
func waitForReaderResume(tunDev *holTun, limit time.Duration) bool {
	start := tunDev.readCalls.Load()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		time.Sleep(holReadPoll)
		if tunDev.readCalls.Load() != start {
			return true
		}
	}
	return false
}

// pushPackets hands n copies of pkt to the TUN, giving up when stop is closed.
func pushPackets(stop <-chan struct{}, tunDev *holTun, pkt []byte, n int) {
	for range n {
		select {
		case tunDev.Outbound <- pkt:
		case <-stop:
			return
		}
	}
}

// waitForPackets waits until counter reaches want or limit elapses and reports
// the value it observed.
func waitForPackets(counter *atomic.Uint64, want uint64, limit time.Duration) uint64 {
	deadline := time.Now().Add(limit)
	for counter.Load() < want && time.Now().Before(deadline) {
		time.Sleep(holReceivePoll)
	}
	return counter.Load()
}

// TestSlowPeerDoesNotStallTUNReader verifies that a peer whose transmission
// path has stalled cannot stop the device from draining its TUN for other
// peers. The kernel TUN ring (tx_queue_len deep) overflows within milliseconds
// once the single reader parks, which is what produced the reported
// tx_dropped growth under bulk traffic.
func TestSlowPeerDoesNotStallTUNReader(t *testing.T) {
	// Registered first so that its check runs after the devices and pushers of
	// this test are torn down.
	goroutineLeakCheck(t)
	pair, stall := newHOLTestPair(t, 1)
	start := time.Now()

	var (
		floodStop   = make(chan struct{})
		pushStop    = make(chan struct{})
		cancelFlood = sync.OnceFunc(func() { close(floodStop) })
		cancelPush  = sync.OnceFunc(func() { close(pushStop) })
		pushers     sync.WaitGroup
		received    atomic.Uint64
	)
	// While the reference bug is present the reader is parked in a blocking
	// handoff, so the stall has to be released (and the pushers stopped) before
	// the devices are closed: shutdown would otherwise block on the parked
	// reader and on the sender waiting for the stalled peer.
	defer func() {
		cancelFlood()
		cancelPush()
		pushers.Wait()
		stall.release()
		if !waitForReaderResume(pair[1].tun, holResumeTimeout) {
			t.Logf("TUN reader did not resume after the stall was released (read calls: %d)", pair[1].tun.readCalls.Load())
		}
	}()

	// Establish the keypairs the test needs before stalling anything.
	pair.exchange(t, 1, 0)
	pair.exchange(t, 1, 2)

	// Device 0's TUN is drained for the rest of the test: its receive path
	// blocks once the inbound channel is full, which must never happen.
	healthy := tuntest.Ping(pair[0].ip, pair[1].ip)
	go func() {
		for {
			select {
			case <-pair[0].tun.closed:
				return
			case pkt := <-pair[0].tun.Inbound:
				if bytes.Equal(pkt, healthy) {
					received.Add(1)
				}
			}
		}
	}()

	// Baseline: with no peer stalled every packet must arrive, so a broken
	// harness cannot pass itself off as the bug under test.
	pushers.Add(1)
	go func() {
		defer pushers.Done()
		pushPackets(pushStop, pair[1].tun, healthy, holHealthyPackets)
	}()
	if got := waitForPackets(&received, holHealthyPackets, holHealthyDeadline); got < holHealthyPackets {
		t.Fatalf("harness check failed: healthy peer received %d/%d packets with no peer stalled", got, holHealthyPackets)
	}
	pushers.Wait()
	received.Store(0)
	t.Logf("baseline: healthy peer received all %d packets with no peer stalled (%v)", holHealthyPackets, time.Since(start))

	// Stall device 1's transmission path to device 2 and fill the handoff
	// pipeline behind it: the reader is expected to park once the per-peer
	// queues are full and the stalled peer never drains them.
	stalled := tuntest.Ping(pair[2].ip, pair[1].ip)
	stagedBefore := perf.TunStagedDropsTotal.Load()
	stall.arm()
	floodFinished := make(chan struct{})
	pushers.Add(1)
	go func() {
		defer pushers.Done()
		defer close(floodFinished)
		floodStalledPeer(floodStop, pair[1].tun, stalled)
	}()
	parked := waitForReaderStall(pair[1].tun, floodFinished, holStallWindow, holStallCap)
	cancelFlood()
	pushers.Wait()
	t.Logf("TUN reader parked while the peer was stalled: %v (read calls: %d, %v)", parked, pair[1].tun.readCalls.Load(), time.Since(start))

	// Packets for the healthy peer arrive behind the stalled peer's traffic in
	// the same TUN: they must still be drained.
	pushers.Add(1)
	go func() {
		defer pushers.Done()
		pushPackets(pushStop, pair[1].tun, healthy, holHealthyPackets)
	}()
	if got := waitForPackets(&received, holHealthyPackets, holHealthyDeadline); got < holHealthyPackets {
		t.Fatalf("healthy peer received %d/%d packets within %v while the other peer's transmission path was stalled; the TUN reader is stuck behind the stalled peer's handoff (read calls: %d, parked: %v)",
			got, holHealthyPackets, holHealthyDeadline, pair[1].tun.readCalls.Load(), parked)
	}
	t.Logf("healthy peer received all %d packets while the other peer was stalled (read calls: %d, %v)", holHealthyPackets, pair[1].tun.readCalls.Load(), time.Since(start))

	// The flood cannot have been absorbed silently: with the stalled peer never
	// draining, the staged handoff must have evicted and counted packets.
	if dropped := perf.TunStagedDropsTotal.Load() - stagedBefore; dropped == 0 {
		t.Fatalf("no staged drops were counted while %d packets for the stalled peer were in flight (total: %d)",
			holFloodPackets, perf.TunStagedDropsTotal.Load())
	} else {
		t.Logf("staged drops during the flood: %d", dropped)
	}
}

// TestSingleQueueFallback verifies that a multi-queue-capable TUN that reports
// a single queue keeps the single-reader/single-writer path, and that packets
// received from the wire still reach the system through the TUN write path.
func TestSingleQueueFallback(t *testing.T) {
	goroutineLeakCheck(t)
	pair, _ := newHOLTestPair(t, 1)
	dev := pair[0].dev

	if dev.tun.multi != nil {
		t.Fatal("device used the multi-queue path for a single-queue TUN")
	}
	if got := dev.tun.queues; got != 1 {
		t.Fatalf("device drains %d TUN queues, want 1", got)
	}
	if got := len(dev.tun.writers); got != 1 {
		t.Fatalf("device has %d TUN writers, want 1", got)
	}

	// The standard exchange, in both directions, over the single-queue path.
	pair.exchange(t, 1, 0)
	pair.exchange(t, 0, 1)

	// A burst received from the peer is handed to the local TUN by the writer
	// goroutine, so it must show up in the TUN's inbound channel.
	const burstPackets = 20
	pair.burst(t, 1, 0, tuntest.Ping(pair[0].ip, pair[1].ip), burstPackets)
	if got := pair[0].tun.written.Load(); got < burstPackets {
		t.Fatalf("device 0 delivered %d packets through the TUN write path, want at least %d", got, burstPackets)
	}
	t.Logf("single-queue fallback delivered %d packets through the TUN write path", pair[0].tun.written.Load())
}

// BenchmarkReaderThroughput measures the outbound TUN datapath end to end for
// MTU-sized packets: TUN reads (one queue per sub-benchmark) -> traffic control
// -> per-peer staged handoff and pump -> encryption -> the bind. The peer's
// bind discards every packet, so the number reported is the rate at which a
// node can drain its TUN towards a peer that is not the bottleneck. It is not a
// measurement of the read syscall alone: everything the datapath does to the
// packet after reading it is in the timed path.
func BenchmarkReaderThroughput(b *testing.B) {
	for _, queues := range []int{1, 4} {
		b.Run(fmt.Sprintf("queues=%d", queues), func(b *testing.B) {
			benchmarkReaderThroughput(b, queues)
		})
	}
}

func benchmarkReaderThroughput(b *testing.B, queues int) {
	b.Helper()

	var devKey, peerKey NoisePrivateKey
	for _, key := range []*NoisePrivateKey{&devKey, &peerKey} {
		if _, err := rand.Read(key[:]); err != nil {
			b.Fatalf("unable to generate private key random bytes: %v", err)
		}
	}
	peerPub := peerKey.publicKey()

	// A synthetic keypair is enough for the outbound pipeline: the sink bind
	// discards every packet, so nothing ever decrypts them and no handshake
	// with a live peer is needed.
	aead, err := chacha20poly1305.New(make([]byte, chacha20poly1305.KeySize))
	if err != nil {
		b.Fatalf("unable to create keypair cipher: %v", err)
	}

	pkt := mtuSizedPacket(benchDst, benchSrc, DefaultMTU)
	sink := &sinkBind{batchSize: conn.IdealBatchSize}
	tin := newHOLTun(queues)
	dev := NewDevice(tin, sink, NewLogger(LogLevelError, "bench: "))
	b.Cleanup(dev.Close)

	if err := dev.IpcSet(uapiCfg(
		"private_key", hex.EncodeToString(devKey[:]),
		"listen_port", "0",
		"replace_peers", "true",
		"public_key", hex.EncodeToString(peerPub[:]),
		"protocol_version", "1",
		"replace_allowed_ips", "true",
		"allowed_ip", benchDst.String()+"/32",
		"endpoint", "127.0.0.1:1",
	)); err != nil {
		b.Fatalf("failed to configure device: %v", err)
	}
	if err := dev.Up(); err != nil {
		b.Fatalf("failed to bring up device: %v", err)
	}
	peer := dev.LookupPeer(peerPub)
	if peer == nil {
		b.Fatal("peer was not configured")
	}
	peer.keypairs.Lock()
	peer.keypairs.current = &Keypair{send: aead, created: time.Now()}
	peer.keypairs.Unlock()

	// Every pushed packet ends up either at the bind or in the staged-drop
	// counter, so waiting for both keeps the loop from spinning on a packet
	// that was legitimately dropped instead of blocking forever.
	stagedBefore := perf.TunStagedDropsTotal.Load()
	accounted := func() uint64 {
		return sink.packets.Load() + perf.TunStagedDropsTotal.Load() - stagedBefore
	}

	// Watchdog: report instead of hanging when the pipeline stops draining.
	stalled := make(chan struct{})
	stopWatchdog := make(chan struct{})
	defer close(stopWatchdog)
	go func() {
		ticker := time.NewTicker(benchStallPoll)
		defer ticker.Stop()
		last := accounted()
		lastChange := time.Now()
		for {
			select {
			case <-stopWatchdog:
				return
			case <-ticker.C:
				if now := accounted(); now != last {
					last, lastChange = now, time.Now()
					continue
				}
				if time.Since(lastChange) >= benchStallTimeout {
					close(stalled)
					return
				}
			}
		}
	}()

	b.SetBytes(int64(len(pkt)))
	b.ResetTimer()
	start := time.Now()
	for pushed := 0; pushed < b.N; {
		select {
		case tin.Outbound <- pkt:
			pushed++
		case <-stalled:
			b.Fatalf("TUN drain stalled after %d/%d packets", pushed, b.N)
		}
	}
	for accounted() < uint64(b.N) {
		select {
		case <-stalled:
			b.Fatalf("TUN drain stalled with %d/%d packets accounted for", accounted(), b.N)
		default:
		}
		time.Sleep(benchDrainPoll)
	}
	elapsed := time.Since(start)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "packets/s")
	b.Logf("queues=%d: drained %d packets in %v (%d staged drops)", queues, b.N, elapsed, perf.TunStagedDropsTotal.Load()-stagedBefore)
}

// benchSrc and benchDst are the addresses the benchmark relays between.
var (
	benchSrc = netip.AddrFrom4([4]byte{1, 0, 0, 1})
	benchDst = netip.AddrFrom4([4]byte{1, 0, 0, 2})
)

// mtuSizedPacket builds an IPv4 packet of exactly size bytes with the given
// destination and source. Only the fields the datapath reads (version, total
// length, addresses) carry meaning: the sink bind discards the payload.
func mtuSizedPacket(dst, src netip.Addr, size int) []byte {
	pkt := make([]byte, size)
	pkt[0] = 4<<4 | 5 // version 4, header length 5 words
	binary.BigEndian.PutUint16(pkt[2:], uint16(size))
	pkt[8] = 64 // TTL
	pkt[9] = 1  // ICMP
	copy(pkt[12:], src.AsSlice())
	copy(pkt[16:], dst.AsSlice())
	return pkt
}
