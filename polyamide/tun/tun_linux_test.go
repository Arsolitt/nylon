//go:build linux

package tun

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCreateTUNWithOptionsMultiQueue(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("creating a TUN device requires root")
	}
	if _, err := os.Stat(cloneDevicePath); err != nil {
		t.Skipf("%s is unavailable: %v", cloneDevicePath, err)
	}

	name := fmt.Sprintf("nyltest%d", rand.IntN(1_000_000))
	dev, err := CreateTUNWithOptions(name, 1420, CreateOptions{Queues: 4, TxQueueLen: 7777})
	if err != nil {
		t.Fatalf("CreateTUNWithOptions(%q, Queues: 4) failed: %v", name, err)
	}
	defer dev.Close()

	mq, ok := dev.(MultiQueueDevice)
	if !ok {
		t.Fatalf("%T does not implement MultiQueueDevice", dev)
	}
	if got := mq.QueueCount(); got != 4 {
		t.Errorf("QueueCount() = %d, want 4", got)
	}
	if got := mq.TxQueueLen(); got != 7777 {
		t.Errorf("TxQueueLen() = %d, want 7777", got)
	}
	if got, err := dev.Name(); err != nil || got != name {
		t.Errorf("Name() = %q, %v; want %q", got, err, name)
	}
	raw, err := os.ReadFile("/sys/class/net/" + name + "/tx_queue_len")
	if err != nil {
		t.Fatalf("reading tx_queue_len: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != "7777" {
		t.Errorf("/sys/class/net/%s/tx_queue_len = %q, want %q", name, got, "7777")
	}
	// Every requested queue must be attached to this interface, not to a
	// separate device created by an attach that missed the name.
	waitForQueues(t, name, 4)

	// An interface created without a name is named by the kernel, and the
	// remaining queues are attached by that name.
	unnamed, err := CreateTUNWithOptions("", 1420, CreateOptions{Queues: 2, TxQueueLen: 7777})
	if err != nil {
		t.Fatalf("CreateTUNWithOptions(\"\", Queues: 2) failed: %v", err)
	}
	defer unnamed.Close()
	unnamedMQ, ok := unnamed.(MultiQueueDevice)
	if !ok {
		t.Fatalf("%T does not implement MultiQueueDevice", unnamed)
	}
	if got := unnamedMQ.QueueCount(); got != 2 {
		t.Errorf("unnamed QueueCount() = %d, want 2", got)
	}
	unnamedName, err := unnamed.Name()
	if err != nil || unnamedName == "" {
		t.Fatalf("Name() = %q, %v; want a kernel assigned name", unnamedName, err)
	}
	waitForQueues(t, unnamedName, 2)

	singleName := fmt.Sprintf("nylsgl%d", rand.IntN(1_000_000))
	single, err := CreateTUNWithOptions(singleName, 1420, CreateOptions{Queues: 1})
	if err != nil {
		t.Fatalf("CreateTUNWithOptions(%q, Queues: 1) failed: %v", singleName, err)
	}
	defer single.Close()
	singleMQ, ok := single.(MultiQueueDevice)
	if !ok {
		t.Fatalf("%T does not implement MultiQueueDevice", single)
	}
	if got := singleMQ.QueueCount(); got != 1 {
		t.Errorf("QueueCount() = %d, want 1", got)
	}
}

// waitForQueues waits until the kernel exposes want transmit queues for the
// interface through sysfs.
func waitForQueues(t *testing.T, name string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if countTxQueues("/sys/class/net/"+name+"/queues") == want {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("interface %s did not expose %d transmit queues", name, want)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func countTxQueues(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	tx := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "tx-") {
			tx++
		}
	}
	return tx
}

func TestNativeTunReadDrainsAvailablePackets(t *testing.T) {
	for _, packetCount := range []int{1, 3} {
		t.Run(strconv.Itoa(packetCount), func(t *testing.T) {
			fds, err := unix.Socketpair(
				unix.AF_UNIX,
				unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC,
				0,
			)
			if err != nil {
				t.Fatal(err)
			}
			reader := os.NewFile(uintptr(fds[0]), "tun-read-test")
			t.Cleanup(func() {
				reader.Close()
				unix.Close(fds[1])
			})

			wantPackets := make([][]byte, packetCount)
			for i := range packetCount {
				wantPackets[i] = []byte{byte(i + 1), byte(i + 11)}
				if _, err := unix.Write(fds[1], wantPackets[i]); err != nil {
					t.Fatal(err)
				}
			}

			const offset = 4
			bufs := make([][]byte, 8)
			sizes := make([]int, len(bufs))
			for i := range bufs {
				bufs[i] = make([]byte, 64)
			}
			tun := &NativeTun{
				queues: []*tunQueue{{
					file:        reader,
					tcpGROTable: newTCPGROTable(),
					udpGROTable: newUDPGROTable(),
				}},
				tunFile: reader,
				errors:  make(chan error),
			}

			count, err := tun.Read(bufs, sizes, offset)
			if err != nil {
				t.Fatal(err)
			}
			if count != packetCount {
				t.Fatalf("Read returned %d packets, want %d", count, packetCount)
			}
			for i, want := range wantPackets {
				if sizes[i] != len(want) {
					t.Errorf("sizes[%d] = %d, want %d", i, sizes[i], len(want))
				}
				if got := bufs[i][offset : offset+sizes[i]]; !bytes.Equal(got, want) {
					t.Errorf("packet %d = %v, want %v", i, got, want)
				}
			}
		})
	}
}

func TestNativeTunReadDefersVirtioFrameThatExceedsRemainingBatch(t *testing.T) {
	fds, err := unix.Socketpair(
		unix.AF_UNIX,
		unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(fds[0]), "tun-virtio-read-test")
	t.Cleanup(func() {
		reader.Close()
		unix.Close(fds[1])
	})

	singlePacket := udp4Packet(ip4PortA, ip4PortB, 16)
	if err := (&virtioNetHdr{}).encode(singlePacket); err != nil {
		t.Fatal(err)
	}
	segmentedPacket := udp4Packet(ip4PortA, ip4PortB, 200)
	segmentedHeader := virtioNetHdr{
		flags:      unix.VIRTIO_NET_HDR_F_NEEDS_CSUM,
		gsoType:    unix.VIRTIO_NET_HDR_GSO_UDP_L4,
		gsoSize:    100,
		hdrLen:     28,
		csumStart:  20,
		csumOffset: 6,
	}
	if err := segmentedHeader.encode(segmentedPacket); err != nil {
		t.Fatal(err)
	}

	const outputOffset = 4
	wantBufs := make([][]byte, 2)
	wantSizes := make([]int, len(wantBufs))
	for i := range wantBufs {
		wantBufs[i] = make([]byte, 256)
	}
	wantCount, err := handleVirtioRead(bytes.Clone(segmentedPacket), wantBufs, wantSizes, outputOffset)
	if err != nil {
		t.Fatal(err)
	}
	if wantCount != len(wantBufs) {
		t.Fatalf("direct virtio split returned %d packets, want %d", wantCount, len(wantBufs))
	}

	for _, packet := range [][]byte{singlePacket, segmentedPacket} {
		if _, err := unix.Write(fds[1], packet); err != nil {
			t.Fatal(err)
		}
	}

	bufs := make([][]byte, 2)
	sizes := make([]int, len(bufs))
	for i := range bufs {
		bufs[i] = make([]byte, 256)
	}
	tun := &NativeTun{
		queues: []*tunQueue{{
			file:        reader,
			tcpGROTable: newTCPGROTable(),
			udpGROTable: newUDPGROTable(),
		}},
		tunFile: reader,
		errors:  make(chan error),
		vnetHdr: true,
	}

	count, err := tun.Read(bufs, sizes, outputOffset)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("first Read returned %d packets, want 1", count)
	}
	if sizes[0] != len(singlePacket)-virtioNetHdrLen {
		t.Fatalf("first packet size = %d, want %d", sizes[0], len(singlePacket)-virtioNetHdrLen)
	}

	count, err = tun.Read(bufs, sizes, outputOffset)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("second Read returned %d packets, want 2", count)
	}
	for i := range wantBufs {
		if sizes[i] != wantSizes[i] {
			t.Errorf("sizes[%d] = %d, want %d", i, sizes[i], wantSizes[i])
			continue
		}
		got := bufs[i][outputOffset : outputOffset+sizes[i]]
		want := wantBufs[i][outputOffset : outputOffset+wantSizes[i]]
		if !bytes.Equal(got, want) {
			t.Errorf("packet %d differs after being deferred", i)
		}
	}
}
