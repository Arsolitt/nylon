/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package tun

/* Implementation of the TUN device interface for linux
 */

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/encodeous/nylon/polyamide/conn"
	"github.com/encodeous/nylon/polyamide/rwcancel"
	"golang.org/x/sys/unix"
)

const (
	cloneDevicePath = "/dev/net/tun"
	ifReqSize       = unix.IFNAMSIZ + 64
	// iffBackpressure is IFF_BACKPRESSURE from uapi/linux/if_tun.h. The kernel
	// then stops the queue instead of dropping when the internal ring is full,
	// so an attached qdisc applies backpressure. Kernels that do not know the
	// flag ignore the bit and do not report it back on TUNGETIFF. It is not
	// defined by golang.org/x/sys/unix yet.
	iffBackpressure = 0x0080
)

// tunQueue is the state of a single TUN queue. A device has at least one.
type tunQueue struct {
	file           *os.File
	readOpMu       sync.Mutex                    // readOpMu guards readBuff and pendingReadLen
	readBuff       [virtioNetHdrLen + 65535]byte // if vnetHdr every read() is prefixed by virtioNetHdr
	pendingReadLen int                           // length of a virtio frame deferred to the next Read

	writeOpMu   sync.Mutex // writeOpMu guards toWrite, tcpGROTable
	toWrite     []int
	tcpGROTable *tcpGROTable
	udpGROTable *udpGROTable
}

type NativeTun struct {
	queues []*tunQueue

	tunFile                 *os.File   // == queues[0].file, the primary queue
	index                   int32      // if index
	errors                  chan error // async error handling
	events                  chan Event // device related events
	netlinkSock             int
	netlinkCancel           *rwcancel.RWCancel
	hackListenerClosed      sync.Mutex
	statusListenersShutdown chan struct{}
	batchSize               int
	vnetHdr                 bool
	udpGSO                  bool
	txQueueLen              int  // effective tx_queue_len read back from the kernel
	backpressure            bool // IFF_BACKPRESSURE accepted by the kernel

	closeOnce sync.Once

	nameOnce  sync.Once // guards calling initNameCache, which sets following fields
	nameCache string    // name of interface
	nameErr   error
}

func (tun *NativeTun) File() *os.File {
	return tun.tunFile
}

func (tun *NativeTun) routineHackListener() {
	defer tun.hackListenerClosed.Unlock()
	/* This is needed for the detection to work across network namespaces
	 * If you are reading this and know a better method, please get in touch.
	 */
	last := 0
	const (
		up   = 1
		down = 2
	)
	for {
		sysconn, err := tun.tunFile.SyscallConn()
		if err != nil {
			return
		}
		err2 := sysconn.Control(func(fd uintptr) {
			_, err = unix.Write(int(fd), nil)
		})
		if err2 != nil {
			return
		}
		switch err {
		case unix.EINVAL:
			if last != up {
				// If the tunnel is up, it reports that write() is
				// allowed but we provided invalid data.
				tun.events <- EventUp
				last = up
			}
		case unix.EIO:
			if last != down {
				// If the tunnel is down, it reports that no I/O
				// is possible, without checking our provided data.
				tun.events <- EventDown
				last = down
			}
		default:
			return
		}
		select {
		case <-time.After(time.Second):
			// nothing
		case <-tun.statusListenersShutdown:
			return
		}
	}
}

func createNetlinkSocket() (int, error) {
	sock, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return -1, err
	}
	saddr := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_LINK | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR,
	}
	err = unix.Bind(sock, saddr)
	if err != nil {
		return -1, err
	}
	return sock, nil
}

func (tun *NativeTun) routineNetlinkListener() {
	defer func() {
		unix.Close(tun.netlinkSock)
		tun.hackListenerClosed.Lock()
		close(tun.events)
		tun.netlinkCancel.Close()
	}()

	for msg := make([]byte, 1<<16); ; {
		var err error
		var msgn int
		for {
			msgn, _, _, _, err = unix.Recvmsg(tun.netlinkSock, msg[:], nil, 0)
			if err == nil || !rwcancel.RetryAfterError(err) {
				break
			}
			if !tun.netlinkCancel.ReadyRead() {
				tun.errors <- fmt.Errorf("netlink socket closed: %w", err)
				return
			}
		}
		if err != nil {
			tun.errors <- fmt.Errorf("failed to receive netlink message: %w", err)
			return
		}

		select {
		case <-tun.statusListenersShutdown:
			return
		default:
		}

		wasEverUp := false
		for remain := msg[:msgn]; len(remain) >= unix.SizeofNlMsghdr; {

			hdr := *(*unix.NlMsghdr)(unsafe.Pointer(&remain[0]))

			if int(hdr.Len) > len(remain) {
				break
			}

			switch hdr.Type {
			case unix.NLMSG_DONE:
				remain = []byte{}

			case unix.RTM_NEWLINK:
				info := *(*unix.IfInfomsg)(unsafe.Pointer(&remain[unix.SizeofNlMsghdr]))
				remain = remain[hdr.Len:]

				if info.Index != tun.index {
					// not our interface
					continue
				}

				if info.Flags&unix.IFF_RUNNING != 0 {
					tun.events <- EventUp
					wasEverUp = true
				}

				if info.Flags&unix.IFF_RUNNING == 0 {
					// Don't emit EventDown before we've ever emitted EventUp.
					// This avoids a startup race with HackListener, which
					// might detect Up before we have finished reporting Down.
					if wasEverUp {
						tun.events <- EventDown
					}
				}

				tun.events <- EventMTUUpdate

			default:
				remain = remain[hdr.Len:]
			}
		}
	}
}

func getIFIndex(name string) (int32, error) {
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFINDEX),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return 0, errno
	}

	return *(*int32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])), nil
}

func (tun *NativeTun) setMTU(n int) error {
	name, err := tun.Name()
	if err != nil {
		return err
	}

	// open datagram socket
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return err
	}

	defer unix.Close(fd)

	// do ioctl call
	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	*(*uint32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = uint32(n)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFMTU),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return fmt.Errorf("failed to set MTU of TUN device: %w", errno)
	}

	return nil
}

func (tun *NativeTun) MTU() (int, error) {
	name, err := tun.Name()
	if err != nil {
		return 0, err
	}

	// open datagram socket
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	// do ioctl call

	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFMTU),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		return 0, fmt.Errorf("failed to get MTU of TUN device: %w", errno)
	}

	return int(*(*int32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ]))), nil
}

// setTxQueueLen sets the tx_queue_len of the interface. The kernel sizes the
// internal ring of each TUN queue from this value when the queue is attached.
func setTxQueueLen(name string, n int) error {
	// open datagram socket
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return err
	}

	defer unix.Close(fd)

	// do ioctl call
	// ifr_qlen is ifru_ivalue, the first int of the ifreq union.
	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	*(*uint32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ])) = uint32(n)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCSIFTXQLEN),
		uintptr(unsafe.Pointer(&ifr[0])),
	)

	if errno != 0 {
		return fmt.Errorf("failed to set tx queue length of TUN device: %w", errno)
	}

	return nil
}

func getTxQueueLen(name string) (int, error) {
	// open datagram socket
	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		0,
	)
	if err != nil {
		return 0, err
	}

	defer unix.Close(fd)

	// do ioctl call

	var ifr [ifReqSize]byte
	copy(ifr[:], name)
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.SIOCGIFTXQLEN),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		return 0, fmt.Errorf("failed to get tx queue length of TUN device: %w", errno)
	}

	return int(*(*int32)(unsafe.Pointer(&ifr[unix.IFNAMSIZ]))), nil
}

func (tun *NativeTun) Name() (string, error) {
	tun.nameOnce.Do(tun.initNameCache)
	return tun.nameCache, tun.nameErr
}

func (tun *NativeTun) initNameCache() {
	tun.nameCache, tun.nameErr = tun.nameSlow()
}

func (tun *NativeTun) nameSlow() (string, error) {
	sysconn, err := tun.tunFile.SyscallConn()
	if err != nil {
		return "", err
	}
	var ifr [ifReqSize]byte
	var errno syscall.Errno
	err = sysconn.Control(func(fd uintptr) {
		_, _, errno = unix.Syscall(
			unix.SYS_IOCTL,
			fd,
			uintptr(unix.TUNGETIFF),
			uintptr(unsafe.Pointer(&ifr[0])),
		)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get name of TUN device: %w", err)
	}
	if errno != 0 {
		return "", fmt.Errorf("failed to get name of TUN device: %w", errno)
	}
	return unix.ByteSliceToString(ifr[:]), nil
}

func (tun *NativeTun) Write(bufs [][]byte, offset int) (int, error) {
	return tun.WriteQueue(0, bufs, offset)
}

// WriteQueue writes bufs to the given TUN queue. Each queue keeps its own
// coalescing state, so queues never share GRO tables.
func (tun *NativeTun) WriteQueue(q int, bufs [][]byte, offset int) (int, error) {
	queue := tun.queues[q]
	queue.writeOpMu.Lock()
	defer func() {
		queue.tcpGROTable.reset()
		queue.udpGROTable.reset()
		queue.writeOpMu.Unlock()
	}()
	var (
		errs  error
		total int
	)
	queue.toWrite = queue.toWrite[:0]
	if tun.vnetHdr {
		err := handleGRO(bufs, offset, queue.tcpGROTable, queue.udpGROTable, tun.udpGSO, &queue.toWrite)
		if err != nil {
			return 0, err
		}
		offset -= virtioNetHdrLen
	} else {
		for i := range bufs {
			queue.toWrite = append(queue.toWrite, i)
		}
	}
	for _, bufsI := range queue.toWrite {
		n, err := queue.file.Write(bufs[bufsI][offset:])
		if errors.Is(err, syscall.EBADFD) {
			return total, os.ErrClosed
		}
		if err != nil {
			errs = errors.Join(errs, err)
		} else {
			total += n
		}
	}
	return total, errs
}

// handleVirtioRead splits in into bufs, leaving offset bytes at the front of
// each buffer. It mutates sizes to reflect the size of each element of bufs,
// and returns the number of packets read.
func handleVirtioRead(in []byte, bufs [][]byte, sizes []int, offset int) (int, error) {
	var hdr virtioNetHdr
	err := hdr.decode(in)
	if err != nil {
		return 0, err
	}
	in = in[virtioNetHdrLen:]
	if hdr.gsoType == unix.VIRTIO_NET_HDR_GSO_NONE {
		if hdr.flags&unix.VIRTIO_NET_HDR_F_NEEDS_CSUM != 0 {
			// This means CHECKSUM_PARTIAL in skb context. We are responsible
			// for computing the checksum starting at hdr.csumStart and placing
			// at hdr.csumOffset.
			err = gsoNoneChecksum(in, hdr.csumStart, hdr.csumOffset)
			if err != nil {
				return 0, err
			}
		}
		if len(in) > len(bufs[0][offset:]) {
			return 0, fmt.Errorf("read len %d overflows bufs element len %d", len(in), len(bufs[0][offset:]))
		}
		n := copy(bufs[0][offset:], in)
		sizes[0] = n
		return 1, nil
	}
	if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV4 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV6 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
		return 0, fmt.Errorf("unsupported virtio GSO type: %d", hdr.gsoType)
	}

	ipVersion := in[0] >> 4
	switch ipVersion {
	case 4:
		if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV4 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
			return 0, fmt.Errorf("ip header version: %d, GSO type: %d", ipVersion, hdr.gsoType)
		}
	case 6:
		if hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_TCPV6 && hdr.gsoType != unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
			return 0, fmt.Errorf("ip header version: %d, GSO type: %d", ipVersion, hdr.gsoType)
		}
	default:
		return 0, fmt.Errorf("invalid ip header version: %d", ipVersion)
	}

	// Don't trust hdr.hdrLen from the kernel as it can be equal to the length
	// of the entire first packet when the kernel is handling it as part of a
	// FORWARD path. Instead, parse the transport header length and add it onto
	// csumStart, which is synonymous for IP header length.
	if hdr.gsoType == unix.VIRTIO_NET_HDR_GSO_UDP_L4 {
		hdr.hdrLen = hdr.csumStart + 8
	} else {
		if len(in) <= int(hdr.csumStart+12) {
			return 0, errors.New("packet is too short")
		}

		tcpHLen := uint16(in[hdr.csumStart+12] >> 4 * 4)
		if tcpHLen < 20 || tcpHLen > 60 {
			// A TCP header must be between 20 and 60 bytes in length.
			return 0, fmt.Errorf("tcp header len is invalid: %d", tcpHLen)
		}
		hdr.hdrLen = hdr.csumStart + tcpHLen
	}

	if len(in) < int(hdr.hdrLen) {
		return 0, fmt.Errorf("length of packet (%d) < virtioNetHdr.hdrLen (%d)", len(in), hdr.hdrLen)
	}

	if hdr.hdrLen < hdr.csumStart {
		return 0, fmt.Errorf("virtioNetHdr.hdrLen (%d) < virtioNetHdr.csumStart (%d)", hdr.hdrLen, hdr.csumStart)
	}
	cSumAt := int(hdr.csumStart + hdr.csumOffset)
	if cSumAt+1 >= len(in) {
		return 0, fmt.Errorf("end of checksum offset (%d) exceeds packet length (%d)", cSumAt+1, len(in))
	}

	return gsoSplit(in, hdr, bufs, sizes, offset, ipVersion == 6)
}

func (tun *NativeTun) readPacket(
	readBuff []byte,
	bufs [][]byte,
	sizes []int,
	offset int,
	read func([]byte) (int, error),
) (int, int, error) {
	readInto := bufs[0][offset:]
	if tun.vnetHdr {
		readInto = readBuff
	}
	n, err := read(readInto)
	if errors.Is(err, syscall.EBADFD) {
		err = os.ErrClosed
	}
	if err != nil {
		return 0, 0, err
	}
	if tun.vnetHdr {
		count, err := handleVirtioRead(readInto[:n], bufs, sizes, offset)
		return count, n, err
	}
	sizes[0] = n
	return 1, n, nil
}

func (tun *NativeTun) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	return tun.ReadQueue(0, bufs, sizes, offset)
}

// ReadQueue reads from the given TUN queue. Each queue is independently
// readable; concurrent reads are only safe on distinct queues.
func (tun *NativeTun) ReadQueue(q int, bufs [][]byte, sizes []int, offset int) (int, error) {
	queue := tun.queues[q]
	queue.readOpMu.Lock()
	defer queue.readOpMu.Unlock()
	select {
	case err := <-tun.errors:
		return 0, err
	default:
	}

	var count int
	var err error
	if queue.pendingReadLen > 0 {
		pendingReadLen := queue.pendingReadLen
		queue.pendingReadLen = 0
		count, err = handleVirtioRead(queue.readBuff[:pendingReadLen], bufs, sizes, offset)
	} else {
		count, _, err = tun.readPacket(queue.readBuff[:], bufs, sizes, offset, queue.file.Read)
	}
	if err != nil {
		return count, err
	}

	rawConn, err := queue.file.SyscallConn()
	if err != nil {
		return count, err
	}
	var drainErr error
	err = rawConn.Control(func(fd uintptr) {
		for count < len(bufs) {
			n, readLen, readErr := tun.readPacket(
				queue.readBuff[:],
				bufs[count:],
				sizes[count:],
				offset,
				func(buf []byte) (int, error) {
					return unix.Read(int(fd), buf)
				},
			)
			if errors.Is(readErr, syscall.EINTR) {
				continue
			}
			if tun.vnetHdr && errors.Is(readErr, ErrTooManySegments) {
				// The frame is already consumed from the TUN device. Keep it
				// in readBuff and split it into a fresh batch on the next Read.
				queue.pendingReadLen = readLen
				return
			}
			count += n
			if errors.Is(readErr, syscall.EAGAIN) || errors.Is(readErr, syscall.EWOULDBLOCK) {
				return
			}
			if readErr != nil {
				drainErr = readErr
				return
			}
		}
	})
	if err != nil {
		return count, err
	}
	return count, drainErr
}

func (tun *NativeTun) Events() <-chan Event {
	return tun.events
}

func (tun *NativeTun) Close() error {
	var err1, err2 error
	tun.closeOnce.Do(func() {
		if tun.statusListenersShutdown != nil {
			close(tun.statusListenersShutdown)
			if tun.netlinkCancel != nil {
				err1 = tun.netlinkCancel.Cancel()
			}
		} else if tun.events != nil {
			close(tun.events)
		}
		err2 = tun.closeQueues()
	})
	if err1 != nil {
		return err1
	}
	return err2
}

// closeQueues closes every queue file of the interface, which detaches the
// queues from the kernel device.
func (tun *NativeTun) closeQueues() error {
	var errs error
	for _, queue := range tun.queues {
		errs = errors.Join(errs, queue.file.Close())
	}
	return errs
}

func (tun *NativeTun) BatchSize() int {
	return tun.batchSize
}

// QueueCount returns the number of TUN queues the device was created with.
func (tun *NativeTun) QueueCount() int {
	return len(tun.queues)
}

// TxQueueLen returns the effective tx_queue_len of the interface, or 0 when it
// could not be read back from the kernel.
func (tun *NativeTun) TxQueueLen() int {
	return tun.txQueueLen
}

// Backpressure reports whether the kernel accepted IFF_BACKPRESSURE.
func (tun *NativeTun) Backpressure() bool {
	return tun.backpressure
}

const (
	// TODO: support TSO with ECN bits
	tunTCPOffloads = unix.TUN_F_CSUM | unix.TUN_F_TSO4 | unix.TUN_F_TSO6
	tunUDPOffloads = unix.TUN_F_USO4 | unix.TUN_F_USO6
)

func (tun *NativeTun) initFromFlags(name string) error {
	sc, err := tun.tunFile.SyscallConn()
	if err != nil {
		return err
	}
	if e := sc.Control(func(fd uintptr) {
		var (
			ifr *unix.Ifreq
		)
		ifr, err = unix.NewIfreq(name)
		if err != nil {
			return
		}
		err = unix.IoctlIfreq(int(fd), unix.TUNGETIFF, ifr)
		if err != nil {
			return
		}
		got := ifr.Uint16()
		// IFF_BACKPRESSURE is only in the flags when the running kernel knows
		// it; older kernels ignore the bit on TUNSETIFF.
		tun.backpressure = got&iffBackpressure != 0
		if got&unix.IFF_VNET_HDR != 0 {
			// tunTCPOffloads were added in Linux v2.6. We require their support
			// if IFF_VNET_HDR is set.
			err = unix.IoctlSetInt(int(fd), unix.TUNSETOFFLOAD, tunTCPOffloads)
			if err != nil {
				return
			}
			tun.vnetHdr = true
			tun.batchSize = conn.IdealBatchSize
			// tunUDPOffloads were added in Linux v6.2. We do not return an
			// error if they are unsupported at runtime.
			tun.udpGSO = unix.IoctlSetInt(int(fd), unix.TUNSETOFFLOAD, tunTCPOffloads|tunUDPOffloads) == nil
		} else {
			tun.batchSize = 1
		}
	}); e != nil {
		return e
	}
	if err != nil {
		return err
	}
	// Best effort: the effective value is reported through TxQueueLen.
	tun.txQueueLen, _ = getTxQueueLen(name)
	return nil
}

// openQueue attaches one queue to the TUN interface using the given TUNSETIFF
// flags and returns the queue file together with the interface name the kernel
// resolved it to (the kernel picks the name when the request comes without
// one, and further queues have to be attached by name).
func openQueue(name string, flags uint16) (*os.File, string, error) {
	nfd, err := unix.Open(cloneDevicePath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("CreateTUN(%q) failed; %s does not exist", name, cloneDevicePath)
		}
		return nil, "", err
	}

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(nfd)
		return nil, "", err
	}
	ifr.SetUint16(flags)
	err = unix.IoctlIfreq(nfd, unix.TUNSETIFF, ifr)
	if err != nil {
		unix.Close(nfd)
		return nil, "", err
	}

	err = unix.SetNonblock(nfd, true)
	if err != nil {
		unix.Close(nfd)
		return nil, "", err
	}

	// Note that the above -- open,ioctl,nonblock -- must happen prior to handing it to netpoll as below this line.

	return os.NewFile(uintptr(nfd), cloneDevicePath), ifr.Name(), nil
}

// CreateTUN creates a Device with the provided name and MTU.
func CreateTUN(name string, mtu int) (Device, error) {
	return CreateTUNWithOptions(name, mtu, DefaultCreateOptions())
}

// CreateTUNWithOptions creates a Device with the provided name, MTU and link
// options. More than one queue requires kernel support for IFF_MULTI_QUEUE;
// when that is unavailable the device falls back to a single queue.
func CreateTUNWithOptions(name string, mtu int, opts CreateOptions) (Device, error) {
	queues := opts.Queues
	if queues < 1 {
		queues = 1
	}
	if queues > 32 {
		queues = 32
	}

	// IFF_VNET_HDR enables the "tun status hack" via routineHackListener()
	// where a null write will return EINVAL indicating the TUN is up.
	flags := uint16(unix.IFF_TUN | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if opts.Backpressure {
		flags |= iffBackpressure
	}

	var fds []*os.File
	if queues > 1 {
		fd, resolved, multiErr := openQueue(name, flags|unix.IFF_MULTI_QUEUE)
		if multiErr != nil {
			// The interface exists without IFF_MULTI_QUEUE, or the kernel does
			// not support it; keep working with a single queue.
			var err error
			fd, _, err = openQueue(name, flags)
			if err != nil {
				// Report both: the fallback alone hides why multi-queue failed.
				return nil, errors.Join(multiErr, err)
			}
			queues = 1
		} else {
			name = resolved
		}
		fds = append(fds, fd)
	} else {
		fd, _, err := openQueue(name, flags)
		if err != nil {
			return nil, err
		}
		fds = append(fds, fd)
	}

	if queues > 1 {
		// The kernel sizes each queue's internal ring from dev->tx_queue_len
		// when the queue is attached, so the length has to be set before the
		// extra queues are attached. The first queue, whose ring was sized at
		// the kernel default, is dropped below.
		if opts.TxQueueLen > 0 {
			// Best effort: without it the kernel default stays in effect.
			_ = setTxQueueLen(name, opts.TxQueueLen)
		}
		for i := 0; i < queues; i++ {
			fd, _, err := openQueue(name, flags|unix.IFF_MULTI_QUEUE)
			if err != nil {
				break
			}
			fds = append(fds, fd)
		}
		if len(fds) > queues {
			// tun_detach() moves the last queue into the freed slot, so every
			// surviving queue keeps the ring depth it was attached with.
			_ = fds[0].Close()
			fds = fds[1:]
		}
	}

	return createTUNFromFiles(fds, mtu)
}

// CreateTUNFromFile creates a Device from an os.File with the provided MTU.
func CreateTUNFromFile(file *os.File, mtu int) (Device, error) {
	return createTUNFromFiles([]*os.File{file}, mtu)
}

// createTUNFromFiles creates a Device from one or more queue files of the same
// TUN interface. The first file is the primary queue.
func createTUNFromFiles(files []*os.File, mtu int) (Device, error) {
	tun := &NativeTun{
		tunFile:                 files[0],
		events:                  make(chan Event, 5),
		errors:                  make(chan error, 5),
		statusListenersShutdown: make(chan struct{}),
	}
	for _, file := range files {
		tun.queues = append(tun.queues, &tunQueue{
			file:        file,
			tcpGROTable: newTCPGROTable(),
			udpGROTable: newUDPGROTable(),
			toWrite:     make([]int, 0, conn.IdealBatchSize),
		})
	}

	name, err := tun.Name()
	if err != nil {
		return nil, err
	}

	err = tun.initFromFlags(name)
	if err != nil {
		return nil, err
	}

	// start event listener
	tun.index, err = getIFIndex(name)
	if err != nil {
		return nil, err
	}

	tun.netlinkSock, err = createNetlinkSocket()
	if err != nil {
		return nil, err
	}
	tun.netlinkCancel, err = rwcancel.NewRWCancel(tun.netlinkSock)
	if err != nil {
		unix.Close(tun.netlinkSock)
		return nil, err
	}

	tun.hackListenerClosed.Lock()
	go tun.routineNetlinkListener()
	go tun.routineHackListener() // cross namespace

	err = tun.setMTU(mtu)
	if err != nil {
		unix.Close(tun.netlinkSock)
		return nil, err
	}

	return tun, nil
}

// CreateUnmonitoredTUNFromFD creates a Device from the provided file
// descriptor.
func CreateUnmonitoredTUNFromFD(fd int) (Device, string, error) {
	err := unix.SetNonblock(fd, true)
	if err != nil {
		return nil, "", err
	}
	file := os.NewFile(uintptr(fd), "/dev/tun")
	tun := &NativeTun{
		tunFile: file,
		events:  make(chan Event, 5),
		errors:  make(chan error, 5),
		queues: []*tunQueue{{
			file:        file,
			tcpGROTable: newTCPGROTable(),
			udpGROTable: newUDPGROTable(),
			toWrite:     make([]int, 0, conn.IdealBatchSize),
		}},
	}
	name, err := tun.Name()
	if err != nil {
		return nil, "", err
	}
	err = tun.initFromFlags(name)
	if err != nil {
		return nil, "", err
	}
	return tun, name, err
}
