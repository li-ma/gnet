// Copyright 2019 Andy Pan. All rights reserved.
// Copyright 2018 Joshua J Baker. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

// +build darwin netbsd freebsd openbsd dragonfly linux

package gnet

import (
	"net"
	"time"

	"github.com/panjf2000/gnet/internal"
	"github.com/panjf2000/gnet/netpoll"
	"github.com/panjf2000/gnet/ringbuffer"
	"golang.org/x/sys/unix"
)

type loop struct {
	idx         int             // loop index in the server loops list
	poller      *netpoll.Poller // epoll or kqueue
	packet      []byte          // read packet buffer
	connections map[int]*conn   // loop connections fd -> conn
	svr         *server
}

func (lp *loop) loopRun() {
	defer lp.svr.signalShutdown()

	if lp.idx == 0 && lp.svr.opts.Ticker {
		go lp.loopTicker()
	}

	_ = lp.poller.Polling(func(fd int, job internal.Job) error {
		if fd == 0 {
			return job()
		}
		if c, ok := lp.connections[fd]; ok {
			switch {
			case !c.opened:
				return lp.loopOpened(c)
			case c.outBuf.Length() > 0:
				return lp.loopWrite(c)
			default:
				return lp.loopRead(c)
			}
		} else {
			return lp.loopAccept(fd)
		}
	})
}

func (lp *loop) loopAccept(fd int) error {
	if fd == lp.svr.ln.fd {
		if lp.svr.ln.pconn != nil {
			return lp.loopUDPRead(fd)
		}
		nfd, sa, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EAGAIN {
				return nil
			}
			return err
		}
		if err := unix.SetNonblock(nfd, true); err != nil {
			return err
		}
		c := &conn{fd: nfd,
			sa:     sa,
			inBuf:  ringbuffer.New(connRingBufferSize),
			outBuf: ringbuffer.New(connRingBufferSize),
			loop:   lp,
		}
		if err = lp.poller.AddReadWrite(c.fd); err == nil {
			lp.connections[c.fd] = c
		} else {
			return err
		}
	}
	return nil
}

func (lp *loop) loopOpened(c *conn) error {
	c.opened = true
	c.localAddr = lp.svr.ln.lnaddr
	c.remoteAddr = netpoll.SockaddrToTCPOrUnixAddr(c.sa)
	out, action := lp.svr.eventHandler.OnOpened(c)
	c.action = action
	if lp.svr.opts.TCPKeepAlive > 0 {
		if _, ok := lp.svr.ln.ln.(*net.TCPListener); ok {
			sniffError(netpoll.SetKeepAlive(c.fd, int(lp.svr.opts.TCPKeepAlive/time.Second)))
		}
	}

	if len(out) > 0 {
		c.open(out)
	}
	if c.outBuf.Length() != 0 {
		_ = lp.poller.AddWrite(c.fd)
	}
	return lp.handleAction(c)
}

func (lp *loop) loopRead(c *conn) error {
	n, err := unix.Read(c.fd, lp.packet)
	if n == 0 || err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		return lp.loopCloseConn(c, err)
	}
	c.extra = lp.packet[:n]
	out, action := lp.svr.eventHandler.React(c)
	c.action = action
	if len(out) > 0 {
		c.write(out)
	} else if action != DataRead {
		_, _ = c.inBuf.Write(c.extra)
	}
	return lp.handleAction(c)
}

func (lp *loop) loopWrite(c *conn) error {
	lp.svr.eventHandler.PreWrite()

	top, tail := c.outBuf.PreReadAll()
	n, err := unix.Write(c.fd, top)
	if err != nil {
		if err == unix.EAGAIN {
			return nil
		}
		return lp.loopCloseConn(c, err)
	}
	c.outBuf.Advance(n)
	if len(top) == n && tail != nil {
		n, err = unix.Write(c.fd, tail)
		if err != nil {
			if err == unix.EAGAIN {
				return nil
			}
			return lp.loopCloseConn(c, err)
		}
		c.outBuf.Advance(n)
	}

	if c.outBuf.Length() == 0 {
		_ = lp.poller.ModRead(c.fd)
	}
	return nil
}

func (lp *loop) loopCloseConn(c *conn, err error) error {
	if err := lp.poller.Delete(c.fd); err == nil {
		delete(lp.connections, c.fd)
		_ = unix.Close(c.fd)
	}

	switch lp.svr.eventHandler.OnClosed(c, err) {
	case None:
	case Shutdown:
		return ErrClosing
	}
	return nil
}

//func (l *loop) loopWake(conn *conn) error {
//	out, action := l.svr.eventHandler.React(conn)
//	conn.action = action
//	if len(out) > 0 {
//		conn.write(out)
//	}
//	return l.handleAction(conn)
//}

//func (l *loop) loopNote(job internal.Job) error {
//
//	var err error
//	switch v := job.(type) {
//	case *conn:
//		l.connections[v.fd] = v
//		l.poller.AddRead(v.fd)
//		return nil
//	case func() error:
//		return v()
//	case time.Duration:
//		delay, action := l.svr.eventHandler.Tick()
//		switch action {
//		case None:
//		case Shutdown:
//			err = ErrClosing
//		}
//		l.svr.tch <- delay
//	case error: // shutdown
//		err = v
//		//case *conn:
//		//	// Wake called for connection
//		//	if val, ok := l.connections[v.fd]; !ok || val != v {
//		//		return nil // ignore stale wakes
//		//	}
//		//	return l.loopWake(v)
//	}
//	return err
//}

func (lp *loop) loopTicker() {
	for {
		if err := lp.poller.Trigger(func() (err error) {
			delay, action := lp.svr.eventHandler.Tick()
			lp.svr.tch <- delay
			switch action {
			case None:
			case Shutdown:
				err = ErrClosing
			}
			return
		}); err != nil {
			break
		}
		time.Sleep(<-lp.svr.tch)
	}
}

func (lp *loop) handleAction(c *conn) error {
	switch c.action {
	case None:
		return nil
	case Close:
		return lp.loopCloseConn(c, nil)
	case Shutdown:
		return ErrClosing
	default:
		return nil
	}
}

func (lp *loop) loopUDPRead(fd int) error {
	n, sa, err := unix.Recvfrom(fd, lp.packet, 0)
	if err != nil || n == 0 {
		return nil
	}
	var sa6 unix.SockaddrInet6
	switch sa := sa.(type) {
	case *unix.SockaddrInet4:
		sa6.ZoneId = 0
		sa6.Port = sa.Port
		for i := 0; i < 12; i++ {
			sa6.Addr[i] = 0
		}
		sa6.Addr[12] = sa.Addr[0]
		sa6.Addr[13] = sa.Addr[1]
		sa6.Addr[14] = sa.Addr[2]
		sa6.Addr[15] = sa.Addr[3]
	case *unix.SockaddrInet6:
		sa6 = *sa
	}
	c := &conn{
		localAddr:  lp.svr.ln.lnaddr,
		remoteAddr: netpoll.SockaddrToUDPAddr(&sa6),
		inBuf:      ringbuffer.New(connRingBufferSize),
	}
	_, _ = c.inBuf.Write(lp.packet[:n])
	out, action := lp.svr.eventHandler.React(c)
	if len(out) > 0 {
		lp.svr.eventHandler.PreWrite()
		sniffError(unix.Sendto(fd, out, 0, sa))
	}
	switch action {
	case Shutdown:
		return ErrClosing
	}
	return nil
}
