/*
 *    HoneyBadger core library for detecting TCP injection attacks
 *
 *    Copyright (C) 2014, 2015  David Stainton
 *
 *    This program is free software: you can redistribute it and/or modify
 *    it under the terms of the GNU General Public License as published by
 *    the Free Software Foundation, either version 3 of the License, or
 *    (at your option) any later version.
 *
 *    This program is distributed in the hope that it will be useful,
 *    but WITHOUT ANY WARRANTY; without even the implied warranty of
 *    MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *    GNU General Public License for more details.
 *
 *    You should have received a copy of the GNU General Public License
 *    along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package HoneyBadger

import (
	"log"
	"sync"
	"time"

	"github.com/david415/HoneyBadger/types"
)

const (
	// Stop looking for handshake hijack after several
	// packets have traversed the connection after entering
	// into TCP_DATA_TRANSFER state
	FIRST_FEW_PACKETS = 12

	// TCP states
	TCP_UNKNOWN                = 0
	TCP_CONNECTION_REQUEST     = 1
	TCP_CONNECTION_ESTABLISHED = 2
	TCP_DATA_TRANSFER          = 3
	TCP_CONNECTION_CLOSING     = 4
	TCP_INVALID                = 5
	TCP_CLOSED                 = 6

	// initiating TCP closing finite state machine
	TCP_FIN_WAIT1 = 0
	TCP_FIN_WAIT2 = 1
	TCP_TIME_WAIT = 2
	TCP_CLOSING   = 3

	// initiated TCP closing finite state machine
	TCP_CLOSE_WAIT = 0
	TCP_LAST_ACK   = 1
)

type ConnectionFactory interface {
	Build(ConnectionOptions) ConnectionInterface
}

type DefaultConnFactory struct {
}

func (f *DefaultConnFactory) Build(options ConnectionOptions) ConnectionInterface {
	conn := Connection{
		packetCount:       0,
		ConnectionOptions: options,
		attackDetected:    false,
		state:             TCP_UNKNOWN,
		skipHijackDetectionCount: FIRST_FEW_PACKETS,
		clientNextSeq:            types.InvalidSequence,
		serverNextSeq:            types.InvalidSequence,
		ClientStreamRing:         types.NewRing(options.MaxRingPackets),
		ServerStreamRing:         types.NewRing(options.MaxRingPackets),
		clientFlow:               &types.TcpIpFlow{},
		serverFlow:               &types.TcpIpFlow{},
	}

	conn.ClientCoalesce = NewOrderedCoalesce(conn.AttackLogger, conn.clientFlow, conn.PageCache, conn.ClientStreamRing, conn.MaxBufferedPagesTotal, conn.MaxBufferedPagesPerConnection/2, conn.DetectCoalesceInjection)
	conn.ServerCoalesce = NewOrderedCoalesce(conn.AttackLogger, conn.serverFlow, conn.PageCache, conn.ServerStreamRing, conn.MaxBufferedPagesTotal, conn.MaxBufferedPagesPerConnection/2, conn.DetectCoalesceInjection)

	return &conn
}

type ConnectionInterface interface {
	Close()
	SetPacketLogger(types.PacketLogger)
	GetConnectionHash() types.ConnectionHash
	GetLastSeen() time.Time
	ReceivePacket(*types.PacketManifest)
}

type PacketDispatcher interface {
	ReceivePacket(*types.PacketManifest)
	GetObservedConnectionsChan(int) chan bool
	Connections() []ConnectionInterface
}

type ConnectionOptions struct {
	MaxBufferedPagesTotal         int
	MaxBufferedPagesPerConnection int
	MaxRingPackets                int
	PageCache                     *pageCache
	LogDir                        string
	LogPackets                    bool
	AttackLogger                  types.Logger
	DetectHijack                  bool
	DetectInjection               bool
	DetectCoalesceInjection       bool
	Pool                          *map[types.ConnectionHash]ConnectionInterface
}

// Connection is used to track client and server flows for a given TCP connection.
// We implement a basic TCP finite state machine and track state in order to detect
// hanshake hijack and other TCP attacks such as segment veto and sloppy injection.
type Connection struct {
	ConnectionOptions
	attackDetected           bool
	packetCount              uint64
	skipHijackDetectionCount uint64
	lastSeen                 time.Time
	lastSeenMutex            sync.Mutex
	state                    uint8
	clientState              uint8
	serverState              uint8
	clientFlow               *types.TcpIpFlow
	serverFlow               *types.TcpIpFlow
	closingFlow              *types.TcpIpFlow
	clientNextSeq            types.Sequence
	serverNextSeq            types.Sequence
	hijackNextAck            types.Sequence
	firstSynAckSeq           uint32
	ClientStreamRing         *types.Ring
	ServerStreamRing         *types.Ring
	ClientCoalesce           *OrderedCoalesce
	ServerCoalesce           *OrderedCoalesce
	PacketLogger             types.PacketLogger
}

func (c *Connection) SetPacketLogger(logger types.PacketLogger) {
	c.PacketLogger = logger
}

func (c *Connection) GetClientStreamRing() *types.Ring {
	return c.ClientStreamRing
}

func (c *Connection) AppendToClientStreamRing(reassembly *types.Reassembly) {
	c.ClientStreamRing.Reassembly = reassembly
	c.ClientStreamRing = c.ClientStreamRing.Next()
}

func (c *Connection) SetServerFlow(flow *types.TcpIpFlow) {
	c.serverFlow = flow
}

func (c *Connection) SetClientFlow(flow *types.TcpIpFlow) {
	c.clientFlow = flow
}

func (c *Connection) getAttackDetectedStatus() bool {
	return c.attackDetected
}

func (c *Connection) setAttackDetectedStatus() {
	c.attackDetected = true
}

func (c *Connection) SetState(state uint8) {
	c.state = state
}

// GetLastSeen returns the lastSeen timestamp after grabbing the lock
func (c *Connection) GetLastSeen() time.Time {
	c.lastSeenMutex.Lock()
	defer c.lastSeenMutex.Unlock()
	return c.lastSeen
}

// updateLastSeen updates our lastSeen with the new timestamp after grabbing the lock
func (c *Connection) updateLastSeen(timestamp time.Time) {
	c.lastSeenMutex.Lock()
	defer c.lastSeenMutex.Unlock()
	if c.lastSeen.Before(timestamp) {
		c.lastSeen = timestamp
	}
}

func (c *Connection) GetConnectionHash() types.ConnectionHash {
	return c.clientFlow.ConnectionHash()
}

// Close can be used by the the connection or the dispatcher to close the connection
func (c *Connection) Close() {
	if c.Pool != nil {
		delete(*c.Pool, c.GetConnectionHash())
	}
	if c.state == TCP_CLOSED {
		panic("already closed")
	}
	c.state = TCP_CLOSED

	if c.getAttackDetectedStatus() == false {
		if c.PacketLogger != nil {
			c.PacketLogger.Remove()
		}
	} else {
		log.Print("not removing logs. attack detected.\n")
	}
	c.ClientCoalesce.Close()
	c.ServerCoalesce.Close()
	if c.LogPackets {
		c.PacketLogger.Stop()
		c.PacketLogger = nil // just in case the state machine receives another packet...
	}
}

// detectHijack checks for duplicate SYN/ACK indicating handshake hijake
// and submits a report if an attack was observed
func (c *Connection) detectHijack(p types.PacketManifest, flow *types.TcpIpFlow) {
	// check for duplicate SYN/ACK indicating handshake hijake
	if !flow.Equal(c.serverFlow) {
		return
	}
	if p.TCP.ACK && p.TCP.SYN {
		if types.Sequence(p.TCP.Ack).Difference(c.hijackNextAck) == 0 {
			if p.TCP.Seq != c.firstSynAckSeq {
				log.Print("handshake hijack detected\n")
				c.AttackLogger.Log(&types.Event{Time: time.Now(), Flow: flow, HijackSeq: p.TCP.Seq, HijackAck: p.TCP.Ack})
				c.setAttackDetectedStatus()
			} else {
				log.Print("SYN/ACK retransmission\n")
			}
		}
	}
}

// detectInjection write an attack report if the given packet indicates a TCP injection attack
// such as segment veto.
func (c *Connection) detectInjection(p types.PacketManifest, flow *types.TcpIpFlow) {
	var ringPtr *types.Ring
	if flow.Equal(c.clientFlow) {
		ringPtr = c.ServerStreamRing
	} else {
		ringPtr = c.ClientStreamRing
	}
	event := injectionInStreamRing(p, flow, ringPtr, "ordered injection", c.packetCount)
	if event != nil {
		c.AttackLogger.Log(event)
		c.setAttackDetectedStatus()
		log.Printf("packet # %d\n", c.packetCount)
	} else {
		log.Print("not an attack attempt; a normal TCP retransmission.\n")
	}
}

// stateUnknown gets called by our TCP finite state machine runtime
// and moves us into the TCP_CONNECTION_REQUEST state if we receive
// a SYN packet... otherwise TCP_DATA_TRANSFER state.
func (c *Connection) stateUnknown(p types.PacketManifest) {
	if p.TCP.SYN && !p.TCP.ACK {
		c.state = TCP_CONNECTION_REQUEST
		*c.clientFlow = *p.Flow
		*c.serverFlow = *p.Flow.Reverse()

		// Note that TCP SYN and SYN/ACK packets may contain payload data if
		// a TCP extension is used...
		// If so then the sequence number needs to track this payload.
		// For more information see: https://tools.ietf.org/id/draft-agl-tcpm-sadata-00.html
		c.clientNextSeq = types.Sequence(p.TCP.Seq).Add(len(p.Payload) + 1) // XXX
		c.hijackNextAck = c.clientNextSeq

	} else {
		// else process a connection after handshake
		c.state = TCP_DATA_TRANSFER
		c.clientFlow = p.Flow
		c.serverFlow = p.Flow.Reverse()

		// skip handshake hijack detection completely
		c.skipHijackDetectionCount = 0
		c.clientNextSeq = types.Sequence(p.TCP.Seq).Add(len(p.Payload) + 1) // XXX

		if p.TCP.FIN || p.TCP.RST {
			log.Print("got RST or FIN")
			c.Close()
		} else {
			if len(p.Payload) > 0 {
				isEnd := false
				c.clientNextSeq, isEnd = c.ServerCoalesce.insert(p, c.clientNextSeq)
				if isEnd {
					c.Close()
				}
			}
		}
	}
}

// stateConnectionRequest gets called by our TCP finite state machine runtime
// and moves us into the TCP_CONNECTION_ESTABLISHED state if we receive
// a SYN/ACK packet.
func (c *Connection) stateConnectionRequest(p types.PacketManifest) {
	if !p.Flow.Equal(c.serverFlow) {
		//handshake anomaly
		c.Close()
		return
	}
	if !(p.TCP.SYN && p.TCP.ACK) {
		//handshake anomaly
		c.Close()
		return
	}
	if c.clientNextSeq.Difference(types.Sequence(p.TCP.Ack)) != 0 {
		//handshake anomaly
		c.Close()
		return
	}
	c.state = TCP_CONNECTION_ESTABLISHED
	c.serverNextSeq = types.Sequence(p.TCP.Seq).Add(len(p.Payload) + 1) // XXX see above comment about TCP extentions
	c.firstSynAckSeq = p.TCP.Seq
}

// stateConnectionEstablished is called by our TCP FSM runtime and
// changes our state to TCP_DATA_TRANSFER if we receive a valid final
// handshake ACK packet.
func (c *Connection) stateConnectionEstablished(p types.PacketManifest) {
	if !c.attackDetected {
		if c.DetectHijack {
			c.detectHijack(p, p.Flow)
			if c.attackDetected {
				return
			}
		}
	}
	if !p.Flow.Equal(c.clientFlow) {
		// handshake anomaly
		c.Close()
		return
	}
	if !p.TCP.ACK || p.TCP.SYN {
		// handshake anomaly
		c.Close()
		return
	}
	if types.Sequence(p.TCP.Seq).Difference(c.clientNextSeq) != 0 {
		// handshake anomaly
		c.Close()
		return
	}
	if types.Sequence(p.TCP.Ack).Difference(c.serverNextSeq) != 0 {
		// handshake anomaly
		c.Close()
		return
	}
	c.state = TCP_DATA_TRANSFER
	log.Printf("connected %s\n", c.clientFlow.String())
}

// stateDataTransfer is called by our TCP FSM and processes packets
// once we are in the TCP_DATA_TRANSFER state
func (c *Connection) stateDataTransfer(p types.PacketManifest) {
	var nextSeqPtr *types.Sequence
	var closerState, remoteState *uint8
	isEnd := false

	if c.clientNextSeq == types.InvalidSequence && p.Flow.Equal(c.clientFlow) {
		c.clientNextSeq, isEnd = c.ServerCoalesce.insert(p, c.clientNextSeq)
		if isEnd {
			c.Close()
		}
		return
	} else if c.serverNextSeq == types.InvalidSequence && p.Flow.Equal(c.serverFlow) {
		c.serverNextSeq, isEnd = c.ClientCoalesce.insert(p, c.serverNextSeq)
		if isEnd {
			c.Close()
		}
		return
	}
	if c.packetCount < c.skipHijackDetectionCount {
		if c.DetectHijack {
			c.detectHijack(p, p.Flow)
		}
	}
	if p.Flow.Equal(c.clientFlow) {
		nextSeqPtr = &c.clientNextSeq
		closerState = &c.clientState
		remoteState = &c.serverState
	} else {
		nextSeqPtr = &c.serverNextSeq
		closerState = &c.serverState
		remoteState = &c.clientState
	}
	diff := types.Sequence(p.TCP.Seq).Difference(*nextSeqPtr)
	// stream overlap case
	if diff > 0 {
		// ignore zero size packets
		if len(p.Payload) > 0 {
			if c.DetectInjection {
				c.detectInjection(p, p.Flow)
			}
		} else {
			// deal with strange packets here...
			// possibly RST or FIN
		}
	} else if diff == 0 { // contiguous
		if p.TCP.RST {
			log.Print("got RST!\n")
			c.Close()
			return
		}
		if len(p.Payload) > 0 {
			reassembly := types.Reassembly{
				Seq:   types.Sequence(p.TCP.Seq),
				Bytes: []byte(p.Payload),
				Seen:  p.Timestamp,
			}
			if p.Flow.Equal(c.clientFlow) {
				c.ServerStreamRing.Reassembly = &reassembly
				c.ServerStreamRing = c.ServerStreamRing.Next()
				*nextSeqPtr = types.Sequence(p.TCP.Seq).Add(len(p.Payload))
				*nextSeqPtr, isEnd = c.ServerCoalesce.addContiguous(*nextSeqPtr)
				if isEnd {
					c.Close()
				}
			} else {
				c.ClientStreamRing.Reassembly = &reassembly
				c.ClientStreamRing = c.ClientStreamRing.Next()
				*nextSeqPtr = types.Sequence(p.TCP.Seq).Add(len(p.Payload))
				*nextSeqPtr, isEnd = c.ClientCoalesce.addContiguous(*nextSeqPtr)
				if isEnd {
					c.Close()
				}
			}
		}
		if p.TCP.FIN {
			//*nextSeqPtr += 1
			c.closingFlow = p.Flow
			c.state = TCP_CONNECTION_CLOSING
			*closerState = TCP_FIN_WAIT1
			*remoteState = TCP_CLOSE_WAIT
			return
		}
	} else if diff < 0 { // future-out-of-order packet case
		if p.Flow.Equal(c.clientFlow) {
			c.clientNextSeq, isEnd = c.ServerCoalesce.insert(p, c.clientNextSeq)
		} else {
			c.serverNextSeq, isEnd = c.ClientCoalesce.insert(p, c.serverNextSeq)
		}
		if isEnd {
			c.Close()
		}
	}
}

// stateFinWait1 handles packets for the FIN-WAIT-1 state
func (c *Connection) stateFinWait1(p types.PacketManifest, flow *types.TcpIpFlow, nextSeqPtr *types.Sequence, nextAckPtr *types.Sequence, statePtr, otherStatePtr *uint8) {
	if p.TCP.ACK {
		*nextAckPtr += 1
		if p.TCP.FIN {
			*statePtr = TCP_CLOSING
			*otherStatePtr = TCP_LAST_ACK
			*nextSeqPtr = types.Sequence(p.TCP.Seq).Add(len(p.Payload) + 1)

			if types.Sequence(p.TCP.Ack).Difference(*nextAckPtr) != 0 {
				log.Printf("FIN-WAIT-1: unexpected ACK: got %d expected %d TCP.Seq %d\n", p.TCP.Ack, *nextAckPtr, p.TCP.Seq)
				c.Close()
				return
			}
		} else {
			*statePtr = TCP_FIN_WAIT2
			*nextSeqPtr = types.Sequence(p.TCP.Seq).Add(len(p.Payload))
		}
	} else {
		log.Print("FIN-WAIT-1: non-ACK packet received.\n")
		c.Close()
	}
}

// stateFinWait2 handles packets for the FIN-WAIT-2 state
func (c *Connection) stateFinWait2(p types.PacketManifest, flow *types.TcpIpFlow, nextSeqPtr *types.Sequence, nextAckPtr *types.Sequence, statePtr *uint8) {
	if types.Sequence(p.TCP.Seq).Difference(*nextSeqPtr) == 0 {
		if p.TCP.ACK && p.TCP.FIN {
			if types.Sequence(p.TCP.Ack).Difference(*nextAckPtr) != 0 {
				log.Print("FIN-WAIT-2: out of order ACK packet received.\n")
				c.Close()
				return
			}
			*nextSeqPtr += 1
			*statePtr = TCP_TIME_WAIT
		} else {
			if len(p.Payload) > 0 {
				c.detectInjection(p, p.Flow)
			} else {
				log.Print("FIN-WAIT-2: injected FIN len 0")
			}
		}
	} else {
		log.Print("FIN-WAIT-2: out of order packet received.\n")
		log.Printf("got TCP.Seq %d expected %d\n", p.TCP.Seq, *nextSeqPtr)
		c.Close()
	}
}

// stateCloseWait represents the TCP FSM's CLOSE-WAIT state
func (c *Connection) stateCloseWait(p types.PacketManifest) {
	var nextSeqPtr *types.Sequence

	if p.Flow.Equal(c.clientFlow) {
		nextSeqPtr = &c.clientNextSeq
	} else {
		nextSeqPtr = &c.serverNextSeq
	}

	diff := types.Sequence(p.TCP.Seq).Difference(*nextSeqPtr)
	// stream overlap case
	if diff > 0 {
		if len(p.Payload) == 0 {
			// XXX perhaps we should count this as an injection?
			// there hasn't been any content injection however this
			// does indicate an attempt to inject a FIN packet
			log.Print("CLOSE-WAIT: injected FIN len 0\n")
		}
	}
}

// stateTimeWait represents the TCP FSM's CLOSE-WAIT state
func (c *Connection) stateTimeWait(p types.PacketManifest) {
	log.Print("TIME-WAIT: invalid protocol state\n")
	c.Close()
}

// stateClosing represents the TCP FSM's CLOSING state
func (c *Connection) stateClosing(p types.PacketManifest) {
	log.Print("CLOSING: invalid protocol state\n")
	c.Close()
}

// stateLastAck represents the TCP FSM's LAST-ACK state
func (c *Connection) stateLastAck(p types.PacketManifest, flow *types.TcpIpFlow, nextSeqPtr *types.Sequence, nextAckPtr *types.Sequence, statePtr *uint8) {
	if types.Sequence(p.TCP.Seq).Difference(*nextSeqPtr) == 0 { //XXX
		if p.TCP.ACK && (!p.TCP.FIN && !p.TCP.SYN) {
			if types.Sequence(p.TCP.Ack).Difference(*nextAckPtr) != 0 {
				log.Printf("LAST-ACK: out of order ACK packet received. seq %d != nextAck %d\n", p.TCP.Ack, *nextAckPtr)
			}
		} else {
			log.Print("LAST-ACK: protocol anamoly\n")
		}
	} else {
		log.Print("LAST-ACK: out of order packet received\n")
		log.Printf("LAST-ACK: out of order packet received; got %d expected %d\n", p.TCP.Seq, *nextSeqPtr)
	}
	c.Close()
}

// stateConnectionClosing handles all the closing states until the closed state has been reached.
func (c *Connection) stateConnectionClosing(p types.PacketManifest) {
	var nextSeqPtr *types.Sequence
	var nextAckPtr *types.Sequence
	var statePtr, otherStatePtr *uint8
	if p.Flow.Equal(c.closingFlow) {
		if c.clientFlow.Equal(p.Flow) {
			statePtr = &c.clientState
			nextSeqPtr = &c.clientNextSeq
			nextAckPtr = &c.serverNextSeq
		} else {
			statePtr = &c.serverState
			nextSeqPtr = &c.serverNextSeq
			nextAckPtr = &c.clientNextSeq
		}
		switch *statePtr {
		case TCP_CLOSE_WAIT:
			c.stateCloseWait(p)
		case TCP_LAST_ACK:
			c.stateLastAck(p, p.Flow, nextSeqPtr, nextAckPtr, statePtr)
		}
	} else {
		if c.clientFlow.Equal(p.Flow) {
			statePtr = &c.clientState
			otherStatePtr = &c.serverState
			nextSeqPtr = &c.clientNextSeq
			nextAckPtr = &c.serverNextSeq
		} else {
			statePtr = &c.serverState
			otherStatePtr = &c.clientState
			nextSeqPtr = &c.serverNextSeq
			nextAckPtr = &c.clientNextSeq
		}
		switch *statePtr {
		case TCP_FIN_WAIT1:
			c.stateFinWait1(p, p.Flow, nextSeqPtr, nextAckPtr, statePtr, otherStatePtr)
		case TCP_FIN_WAIT2:
			c.stateFinWait2(p, p.Flow, nextSeqPtr, nextAckPtr, statePtr)
		case TCP_TIME_WAIT:
			c.stateTimeWait(p)
		case TCP_CLOSING:
			c.stateClosing(p)
		}
	}
}

// ReceivePacket implements a TCP finite state machine
// which is loosely based off of the simplified FSM in this paper:
// http://ants.iis.sinica.edu.tw/3bkmj9ltewxtsrrvnoknfdxrm3zfwrr/17/p520460.pdf
// The goal is to detect all manner of content injection.
func (c *Connection) ReceivePacket(p *types.PacketManifest) {
	c.updateLastSeen(p.Timestamp)
	if c.PacketLogger != nil {
		c.PacketLogger.WritePacket(p.RawPacket, p.Timestamp)
	}
	c.packetCount += 1
	switch c.state {
	case TCP_UNKNOWN:
		c.stateUnknown(*p)
	case TCP_CONNECTION_REQUEST:
		c.stateConnectionRequest(*p)
	case TCP_CONNECTION_ESTABLISHED:
		c.stateConnectionEstablished(*p)
	case TCP_DATA_TRANSFER:
		c.stateDataTransfer(*p)
	case TCP_CONNECTION_CLOSING:
		c.stateConnectionClosing(*p)
	case TCP_CLOSED:
		log.Print("connection closed; ignoring received packet.")
		return
	}
}
