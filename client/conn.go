// Copyright (c) 2022 Institute of Software, Chinese Academy of Sciences (ISCAS)
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/isrc-cas/gt/bufio"
	"github.com/isrc-cas/gt/client/api"
	connection "github.com/isrc-cas/gt/conn"
	"github.com/isrc-cas/gt/pool"
	"github.com/isrc-cas/gt/predef"
)

type conn struct {
	connection.Connection
	client        *Client
	tasks         map[uint32]*httpTask
	finishedTasks atomic.Uint64
	tasksRWMtx    sync.RWMutex
	stuns         []string
	service       *service
}

func newConn(c net.Conn, client *Client) *conn {
	nc := &conn{
		Connection: connection.Connection{
			Conn:         c,
			Reader:       pool.GetReader(c),
			WriteTimeout: client.config.RemoteTimeout,
		},
		client: client,
		tasks:  make(map[uint32]*httpTask, 100),
	}
	return nc
}

func (c *conn) init() (err error) {
	buf := c.Connection.Reader.GetBuf()
	bufIndex := 0

	buf[bufIndex] = predef.MagicNumber
	bufIndex++
	buf[bufIndex] = 0x01 // version
	bufIndex++

	// id
	buf[bufIndex] = byte(len(c.client.config.ID))
	bufIndex++
	idLen := copy(buf[bufIndex:], c.client.config.ID)
	bufIndex += idLen

	// secret
	buf[bufIndex] = byte(len(c.client.config.Secret))
	bufIndex++
	secretLen := copy(buf[bufIndex:], c.client.config.Secret)
	bufIndex += secretLen

	// services
	for i, service := range c.client.services {
		if i != len(c.client.services)-1 {
			optionLen := copy(buf[bufIndex:], predef.OptionAndNextOption)
			bufIndex += optionLen
		}
		if service.LocalURL.Scheme == "tcp" {
			optionLen := copy(buf[bufIndex:], predef.OpenTCPPort)
			bufIndex += optionLen

			if *service.RemoteTCPRandom {
				buf[bufIndex] = 1
			} else {
				buf[bufIndex] = 0
			}
			bufIndex++

			buf[bufIndex] = byte(service.RemoteTCPPort >> 8)
			buf[bufIndex+1] = byte(service.RemoteTCPPort)
			bufIndex += 2
		} else if service.HostPrefix == c.client.config.ID {
			optionLen := copy(buf[bufIndex:], predef.IDAsHostPrefix)
			bufIndex += optionLen
		} else {
			optionLen := copy(buf[bufIndex:], predef.OpenHost)
			bufIndex += optionLen

			buf[bufIndex] = byte(len(service.HostPrefix))
			bufIndex++
			hostPrefixLen := copy(buf[bufIndex:], service.HostPrefix)
			bufIndex += hostPrefixLen
		}
	}

	_, err = c.Conn.Write(buf[:bufIndex])

	return
}

func (c *conn) IsTimeout(e error) (result bool) {
	if ne, ok := e.(*net.OpError); ok && ne.Timeout() {
		err := c.Connection.SendPingSignal()
		if err == nil {
			result = true
			return
		}
		c.Logger.Debug().Err(err).Msg("failed to send ping signal")
	}
	return
}

func (c *conn) Close() {
	if !c.Closing.CompareAndSwap(0, 1) {
		return
	}
	c.tasksRWMtx.Lock()
	for _, task := range c.tasks {
		task.Close()
	}
	c.tasksRWMtx.Unlock()
	c.Connection.CloseOnce()
}

func (c *conn) tasksLen() (n int) {
	c.tasksRWMtx.RLock()
	n = len(c.tasks)
	c.tasksRWMtx.RUnlock()
	return n
}

func (c *conn) readLoop(connID uint) {
	var err error
	var pings int
	var lastPing int
	var isClosing bool
	defer func() {
		c.client.removeTunnel(c)
		c.Close()
		c.Logger.Info().Err(err).Bool("isClosing", isClosing).Uint64("finishedTasks", c.finishedTasks.Load()).
			Int("tasksCount", c.tasksLen()).Int("pings", pings).Msg("tunnel closed")
		c.onTunnelClose()
		pool.PutReader(c.Reader)
	}()

	r := &bufio.LimitedReader{}
	r.Reader = c.Reader
	var timeout time.Duration
	if c.client.config.RemoteTimeout > 0 {
		timeout = c.client.config.RemoteTimeout / 2
		if timeout <= 0 {
			timeout = c.client.config.RemoteTimeout
		}
	}
	for pings <= 3 {
		if timeout > 0 {
			err = c.Conn.SetReadDeadline(time.Now().Add(timeout))
			if err != nil {
				return
			}
		}
		var peekBytes []byte
		peekBytes, err = c.Reader.Peek(4)
		if err != nil {
			if c.IsTimeout(err) {
				pings++
				c.Logger.Info().Bool("isClosing", isClosing).Uint64("finishedTasks", c.finishedTasks.Load()).
					Int("tasksCount", c.tasksLen()).Int("pings", pings).Msg("sent ping")
				err = nil
				continue
			}
			return
		}
		signal := uint32(peekBytes[3]) | uint32(peekBytes[2])<<8 | uint32(peekBytes[1])<<16 | uint32(peekBytes[0])<<24
		_, err = c.Reader.Discard(4)
		if err != nil {
			return
		}
		switch signal {
		case connection.PingSignal:
			pings--
			lastPing++
			if isClosing && lastPing >= 3 {
				if c.tasksLen() == 0 {
					return
				}
			}
			if lastPing >= 6 {
				lastPing = 0
				if c.client.idleManager.ChangeToWait(connID) {
					c.SendCloseSignal()
					c.Logger.Info().Msg("sent close signal")
				}
			}
			continue
		case connection.CloseSignal:
			c.Logger.Info().Msg("read close signal")
			if isClosing {
				return
			}
			isClosing = true
			continue
		case connection.ReadySignal:
			c.client.addTunnel(c)
			c.Logger.Info().Msg("tunnel started")
			continue
		case connection.ErrorSignal:
			peekBytes, err = c.Reader.Peek(2)
			if err != nil {
				return
			}
			errCode := uint16(peekBytes[1]) | uint16(peekBytes[0])<<8
			c.Logger.Error().Err(connection.Error(errCode)).Msg("read error signal")
			return
		case connection.InfoSignal:
			peekBytes, err = c.Reader.Peek(2)
			if err != nil {
				return
			}
			infoCode := uint16(peekBytes[1]) | uint16(peekBytes[0])<<8
			_, err = c.Reader.Discard(2)
			if err != nil {
				return
			}
			info, err := connection.Info(infoCode).ReadInfo(c.Reader)
			if err != nil {
				return
			}
			c.Logger.Info().Msgf("receive server information: %s", info)
			continue
		}
		lastPing = 0
		taskID := signal
		peekBytes, err = c.Reader.Peek(2)
		if err != nil {
			return
		}
		taskOption := uint16(peekBytes[1]) | uint16(peekBytes[0])<<8
		_, err = c.Reader.Discard(2)
		if err != nil {
			return
		}
		switch taskOption {
		case predef.Data:
			fallthrough
		case predef.ServicesData:
			serviceIndex := uint16(0)
			if taskOption != predef.Data {
				peekBytes, err = c.Reader.Peek(2)
				if err != nil {
					return
				}
				serviceIndex = uint16(peekBytes[1]) | uint16(peekBytes[0])<<8
				_, err = c.Reader.Discard(2)
				if err != nil {
					return
				}
			}
			if serviceIndex >= uint16(len(c.client.services)) {
				c.Logger.Error().Msg("invalid service index")
				return
			}
			c.service = &c.client.services[serviceIndex]

			peekBytes, err = c.Reader.Peek(4)
			if err != nil {
				return
			}
			l := uint32(peekBytes[3]) | uint32(peekBytes[2])<<8 | uint32(peekBytes[1])<<16 | uint32(peekBytes[0])<<24
			_, err = c.Reader.Discard(4)
			if err != nil {
				return
			}
			r.N = int64(l)
			rErr, wErr := c.processData(connID, taskID, r)
			if rErr != nil {
				err = wErr
				if !errors.Is(rErr, net.ErrClosed) {
					c.Logger.Warn().Err(rErr).Msg("failed to read data in processData")
				}
				return
			}
			if r.N > 0 {
				_, err = r.Discard(int(r.N))
				if err != nil {
					return
				}
			}
			if wErr != nil {
				if !errors.Is(wErr, net.ErrClosed) {
					c.Logger.Warn().Err(wErr).Msg("failed to write data in processData")
				}
				continue
			}
		case predef.Close:
			c.tasksRWMtx.RLock()
			t, ok := c.tasks[taskID]
			c.tasksRWMtx.RUnlock()
			if ok {
				t.CloseByRemote()
			}
		}
	}
}

func (c *conn) dial() (task *httpTask, err error) {
	conn, err := net.Dial("tcp", c.service.LocalURL.Host)
	if err != nil {
		return
	}
	task = newHTTPTask(conn)
	if c.service.UseLocalAsHTTPHost {
		err = task.setHost(c.service.LocalURL.Host)
	}
	return
}

func (c *conn) processData(connID uint, taskID uint32, r *bufio.LimitedReader) (readErr, writeErr error) {
	c.tasksRWMtx.RLock()
	task, ok := c.tasks[taskID]
	c.tasksRWMtx.RUnlock()
	if !ok {
		var peekBytes []byte
		peekBytes, readErr = r.Peek(2)
		if readErr != nil {
			return
		}
		// first 2 bytes of p2p sdp request is "XP"(0x5850)
		isP2P := (uint16(peekBytes[1]) | uint16(peekBytes[0])<<8) == 0x5850
		c.client.peersRWMtx.RLock()
		pt, ok := c.client.peers[taskID]
		c.client.peersRWMtx.RUnlock()
		if pt != nil || (isP2P && !ok) {
			if len(c.stuns) < 1 {
				respAndClose(taskID, c, [][]byte{
					[]byte("HTTP/1.1 403 Forbidden\r\nConnection: Closed\r\n\r\n"),
				})
				return
			}
			c.processP2P(taskID, r, pt, ok)
			return
		}

		for i := 0; i < 3; i++ {
			task, writeErr = c.dial()
			if writeErr == nil {
				break
			}
		}
		if writeErr != nil {
			return
		}
		task.Logger = c.Logger.With().
			Uint32("task", taskID).
			Logger()
		task.Logger.Info().Msg("task started")
		c.tasksRWMtx.Lock()
		c.tasks[taskID] = task
		c.tasksRWMtx.Unlock()
		go task.process(connID, taskID, c, c.service.LocalTimeout)
	}
	_, err := r.WriteTo(task)
	if err != nil {
		switch e := err.(type) {
		case *net.OpError:
			switch e.Op {
			case "write":
				writeErr = err
			}
		case *bufio.WriteErr:
			writeErr = err
		default:
			readErr = err
		}
	}
	if c.service.LocalTimeout > 0 {
		dl := time.Now().Add(c.service.LocalTimeout)
		writeErr = task.conn.SetReadDeadline(dl)
		if writeErr != nil {
			return
		}
	}
	return
}

func (c *conn) processP2P(id uint32, r *bufio.LimitedReader, t *peerTask, ok bool) {
	if !ok {
		t = &peerTask{}
		t.id = id
		t.tunnel = c
		t.apiConn = api.NewConn(id, "", c)
		t.apiConn.ProcessOffer = t.processOffer
		t.apiConn.GetOffer = t.getOffer
		t.apiConn.ProcessAnswer = t.processAnswer
		t.data = pool.BytesPool.Get().([]byte)
		t.candidateOutChan = make(chan string, 16)
		t.closeChan = make(chan struct{})
		t.waitNegotiationNeeded = make(chan struct{})
		t.Logger = c.Logger.With().
			Uint32("peerTask", id).
			Logger()
		t.timer = time.AfterFunc(120*time.Second, func() {
			t.Logger.Info().Msg("peer task timeout")
			t.CloseWithLock()
		})

		c.client.peersRWMtx.Lock()
		c.client.peers[id] = t
		c.client.peersRWMtx.Unlock()

		c.client.apiServer.Listener.AcceptCh() <- t.apiConn
		t.Logger.Info().Msg("peer task started")
	}
	_, err := r.WriteTo(t.apiConn.PipeWriter)
	if err != nil {
		t.Logger.Error().Err(err).Msg("processP2P WriteTo failed")
	}
}
