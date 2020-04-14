// Copyright 2020 The gVisor Authors.
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

// Package testbench has utilities to send and receive packets and also command
// the DUT to run POSIX functions.
package testbench

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"testing"
	"time"

	"github.com/mohae/deepcopy"
	"go.uber.org/multierr"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/seqnum"
)

var localIPv4 = flag.String("local_ipv4", "", "local IPv4 address for test packets")
var remoteIPv4 = flag.String("remote_ipv4", "", "remote IPv4 address for test packets")
var localMAC = flag.String("local_mac", "", "local mac address for test packets")
var remoteMAC = flag.String("remote_mac", "", "remote mac address for test packets")

// pickPort makes a new socket and returns the socket FD and port. The caller
// must close the FD when done with the port if there is no error.
func pickPort() (int, uint16, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		return -1, 0, err
	}
	var sa unix.SockaddrInet4
	copy(sa.Addr[0:4], net.ParseIP(*localIPv4).To4())
	if err := unix.Bind(fd, &sa); err != nil {
		unix.Close(fd)
		return -1, 0, err
	}
	newSockAddr, err := unix.Getsockname(fd)
	if err != nil {
		unix.Close(fd)
		return -1, 0, err
	}
	newSockAddrInet4, ok := newSockAddr.(*unix.SockaddrInet4)
	if !ok {
		unix.Close(fd)
		return -1, 0, fmt.Errorf("can't cast Getsockname result to SockaddrInet4")
	}
	return fd, uint16(newSockAddrInet4.Port), nil
}

// layerState stores the state of a layer of a connection.
type layerState interface {
	// outgoing returns an outgoing layer to be sent in a frame.
	outgoing() Layer

	// incoming creates an expected Layer for comparing against a received Layer.
	// Because the expectation can depend on values in the received Layer, it is
	// an input to incoming. For example, the ACK number needs to be checked in a
	// TCP packet but only if the ACK flag is set in the received packet.
	incoming(received Layer) Layer

	// sent updates the layerState based on a frame that is sent. The input is a
	// Layer with all prev and next pointers populated so that the entire frame as
	// it was sent is available.
	sent(Layer) error

	// received updates the layerState based on a frame that is receieved. The
	// input is a Layer with all prev and next pointers populated so that the
	// entire frame as it was receieved is available.
	received(Layer) error

	// close cleans up any resources held.
	close() error
}

// etherState maintains state about an Ethernet connection.
type etherState struct {
	out, in Ether
}

// newEtherState creates a new etherState.
func newEtherState(out, in Ether) (*etherState, error) {
	lMAC, err := tcpip.ParseMACAddress(*localMAC)
	if err != nil {
		return nil, err
	}

	rMAC, err := tcpip.ParseMACAddress(*remoteMAC)
	if err != nil {
		return nil, err
	}
	s := etherState{
		out: Ether{SrcAddr: &lMAC, DstAddr: &rMAC},
		in:  Ether{SrcAddr: &rMAC, DstAddr: &lMAC},
	}
	if err := s.out.merge(&out); err != nil {
		return nil, err
	}
	if err := s.in.merge(&in); err != nil {
		return nil, err
	}
	return &s, nil
}

// outgoing returns an outgoing layer to be sent in a frame.
func (s *etherState) outgoing() Layer {
	return &s.out
}

func (s *etherState) incoming(Layer) Layer {
	return deepcopy.Copy(&s.in).(Layer)
}

func (*etherState) sent(Layer) error {
	// Nothing to do.
	return nil
}

func (*etherState) received(Layer) error {
	// Nothing to do.
	return nil
}

// Close cleans up any resources held.
func (s *etherState) close() error {
	return nil
}

// ipv4State maintains state about an IPv4 connection.
type ipv4State struct {
	out, in IPv4
}

// newIPv4State creates a new ipv4State.
func newIPv4State(out, in IPv4) (*ipv4State, error) {
	lIP := tcpip.Address(net.ParseIP(*localIPv4).To4())
	rIP := tcpip.Address(net.ParseIP(*remoteIPv4).To4())
	s := ipv4State{
		out: IPv4{SrcAddr: &lIP, DstAddr: &rIP},
		in:  IPv4{SrcAddr: &rIP, DstAddr: &lIP},
	}
	if err := s.out.merge(&out); err != nil {
		return nil, err
	}
	if err := s.in.merge(&in); err != nil {
		return nil, err
	}
	return &s, nil
}

// outgoing returns an outgoing layer to be sent in a frame.
func (s *ipv4State) outgoing() Layer {
	return &s.out
}

func (s *ipv4State) incoming(Layer) Layer {
	return deepcopy.Copy(&s.in).(Layer)
}

func (*ipv4State) sent(Layer) error {
	// Nothing to do.
	return nil
}

func (*ipv4State) received(Layer) error {
	// Nothing to do.
	return nil
}

// Close cleans up any resources held.
func (s *ipv4State) close() error {
	return nil
}

// tcpState maintains state about a TCP connection.
type tcpState struct {
	out, in                   TCP
	localSeqNum, remoteSeqNum *seqnum.Value
	synAck                    *TCP
	portPickerFD              int
}

// SeqNumValue is a helper routine that allocates a new seqnum.Value value to
// store v and returns a pointer to it.
func SeqNumValue(v seqnum.Value) *seqnum.Value {
	return &v
}

// newTCPState creates a new TCPState.
func newTCPState(out, in TCP) (*tcpState, error) {
	portPickerFD, localPort, err := pickPort()
	if err != nil {
		return nil, err
	}
	s := tcpState{
		out:          TCP{SrcPort: &localPort},
		in:           TCP{DstPort: &localPort},
		localSeqNum:  SeqNumValue(seqnum.Value(rand.Uint32())),
		portPickerFD: portPickerFD,
	}
	if err := s.out.merge(&out); err != nil {
		return nil, err
	}
	if err := s.in.merge(&in); err != nil {
		return nil, err
	}
	return &s, nil
}

// outgoing returns an outgoing layer to be sent in a frame.
func (s *tcpState) outgoing() Layer {
	newOutgoing := deepcopy.Copy(s.out).(TCP)
	if s.localSeqNum != nil {
		newOutgoing.SeqNum = Uint32(uint32(*s.localSeqNum))
	}
	if s.remoteSeqNum != nil {
		newOutgoing.AckNum = Uint32(uint32(*s.remoteSeqNum))
	}
	return &newOutgoing
}

func (s *tcpState) incoming(received Layer) Layer {
	tcpReceived, ok := received.(*TCP)
	if !ok {
		return nil
	}
	newIn := deepcopy.Copy(s.in).(TCP)
	if s.remoteSeqNum != nil {
		newIn.SeqNum = Uint32(uint32(*s.remoteSeqNum))
	}
	if s.localSeqNum != nil && (*tcpReceived.Flags&header.TCPFlagAck) != 0 {
		// The caller didn't specify an AckNum so we'll expect the calculated one,
		// but only if the ACK flag is set because the AckNum is not valid in a
		// header if ACK is not set.
		newIn.AckNum = Uint32(uint32(*s.localSeqNum))
	}
	return &newIn
}

func (s *tcpState) sent(l Layer) error {
	tcp, ok := l.(*TCP)
	if !ok {
		return fmt.Errorf("can't update tcpState with %T Layer", l)
	}
	for current := tcp.next(); current != nil; current = current.next() {
		s.localSeqNum.UpdateForward(seqnum.Size(current.length()))
	}
	if tcp.Flags != nil && *tcp.Flags&(header.TCPFlagSyn|header.TCPFlagFin) != 0 {
		s.localSeqNum.UpdateForward(1)
	}
	return nil
}

func (s *tcpState) received(l Layer) error {
	tcp, ok := l.(*TCP)
	if !ok {
		return fmt.Errorf("can't update tcpState with %T Layer", l)
	}
	s.remoteSeqNum = SeqNumValue(seqnum.Value(*tcp.SeqNum))
	if *tcp.Flags&(header.TCPFlagSyn|header.TCPFlagFin) != 0 {
		s.remoteSeqNum.UpdateForward(1)
	}
	for current := tcp.next(); current != nil; current = current.next() {
		s.remoteSeqNum.UpdateForward(seqnum.Size(current.length()))
	}
	return nil
}

// Close the port associated with this connection.
func (s *tcpState) close() error {
	if err := unix.Close(s.portPickerFD); err != nil {
		return err
	}
	s.portPickerFD = -1
	return nil
}

// udpState maintains state about a UDP connection.
type udpState struct {
	out, in      UDP
	portPickerFD int
}

// newUDPState creates a new udpState.
func newUDPState(out, in UDP) (*udpState, error) {
	portPickerFD, localPort, err := pickPort()
	if err != nil {
		return nil, err
	}
	s := udpState{
		out:          UDP{SrcPort: &localPort},
		in:           UDP{DstPort: &localPort},
		portPickerFD: portPickerFD,
	}
	if err := s.out.merge(&out); err != nil {
		return nil, err
	}
	if err := s.in.merge(&in); err != nil {
		return nil, err
	}
	return &s, nil
}

// outgoing returns an outgoing layer to be sent in a frame.
func (s *udpState) outgoing() Layer {
	return &s.out
}

func (s *udpState) incoming(Layer) Layer {
	return deepcopy.Copy(&s.in).(Layer)
}

func (*udpState) sent(l Layer) error {
	// Nothing to do.
	return nil
}

func (*udpState) received(l Layer) error {
	// Nothing to do.
	return nil
}

// Close the port associated with this connection.
func (s *udpState) close() error {
	if err := unix.Close(s.portPickerFD); err != nil {
		return err
	}
	s.portPickerFD = -1
	return nil
}

// Connection holds a collection of layer states for maintaining a connection
// along with sockets for sniffer and injecting packets.
type Connection struct {
	layerStates []layerState
	injector    Injector
	sniffer     Sniffer
	t           *testing.T
}

// Returns the default incoming frame against which to match. If received is
// longer than layerStates then that may still count as a match. The reverse is
// never a match and nil is returned.
func (conn *Connection) incoming(received Layers) Layers {
	if len(received) < len(conn.layerStates) {
		return nil
	}
	in := Layers{}
	for i, s := range conn.layerStates {
		toMatch := s.incoming(received[i])
		if toMatch == nil {
			return nil
		}
		in = append(in, toMatch)
	}
	return in
}

// Close cleans up any resources held.
func (conn *Connection) Close() {
	errs := multierr.Combine(conn.sniffer.close(), conn.injector.close())
	for _, s := range conn.layerStates {
		if err := s.close(); err != nil {
			errs = multierr.Append(errs, fmt.Errorf("unable to close %v: %s", s, err))
		}
	}
	if errs != nil {
		conn.t.Fatalf("unable to close %v: %s", conn, errs)
	}
}

// CreateFrame builds a frame for the connection with layer overriding defaults
// of the innermost layer and additionalLayers added after it.
func (conn *Connection) CreateFrame(layer Layer, additionalLayers ...Layer) Layers {
	var layersToSend Layers
	for _, s := range conn.layerStates {
		layersToSend = append(layersToSend, s.outgoing())
	}
	if err := layersToSend[len(layersToSend)-1].merge(layer); err != nil {
		conn.t.Fatalf("can't merge %+v into %+v: %s", layer, layersToSend[len(layersToSend)-1], err)
	}
	layersToSend = append(layersToSend, additionalLayers...)
	return layersToSend
}

// SendFrame sends a frame on the wire and updates the state of all layers.
func (conn *Connection) SendFrame(frame Layers) {
	outBytes, err := frame.toBytes()
	if err != nil {
		conn.t.Fatalf("can't build outgoing TCP packet: %s", err)
	}
	conn.injector.Send(outBytes)

	// frame might have nil values where the caller wanted to use default values.
	// sentFrame will have no nil values in it because it comes from parsing the
	// bytes that were actually sent.
	sentFrame := parse(parseEther, outBytes)
	// Update the state of each layer based on what was sent.
	for i, s := range conn.layerStates {
		if err := s.sent(sentFrame[i]); err != nil {
			conn.t.Fatal(err)
		}
	}
}

// Send a packet with reasonable defaults. Potentially override the final layer
// in the connection with the provided layer and add additionLayers.
func (conn *Connection) Send(layer Layer, additionalLayers ...Layer) {
	conn.SendFrame(conn.CreateFrame(layer, additionalLayers...))
}

// recvFrame gets the next successfully parsed frame (of type Layers) within the
// timeout provided. If no parsable frame arrives before the timeout, it returns
// nil.
func (conn *Connection) recvFrame(timeout time.Duration) Layers {
	if timeout <= 0 {
		return nil
	}
	b := conn.sniffer.Recv(timeout)
	if b == nil {
		return nil
	}
	return parse(parseEther, b)
}

// LayersError stores the Layers that we got and the Layers that we wanted to
// match.
type LayersError struct {
	got  Layers
	want Layers
}

func (e LayersError) Error() string {
	return e.got.diff(e.want)
}

// Expect a frame with the final layerStates layer matching the provided Layer
// within the timeout specified. If it doesn't arrive in time, it returns nil.
func (conn *Connection) Expect(layer Layer, timeout time.Duration) (Layer, error) {
	// Make a frame that will ignore all but the final layer.
	layers := make([]Layer, len(conn.layerStates))
	layers[len(layers)-1] = layer

	gotFrame, err := conn.ExpectFrame(layers, timeout)
	if err != nil {
		return nil, err
	}
	if len(conn.layerStates)-1 < len(gotFrame) {
		return gotFrame[len(conn.layerStates)-1], nil
	}
	conn.t.Fatal("the received frame should be at least as long as the expected layers")
	return nil, &LayersError{}
}

// ExpectFrame expects a frame that matches the provided Layers within the
// timeout specified. If one arrives in time, the Layers is returned without an
// error. If it doesn't arrive in time, it returns nil and error is non-nil.
func (conn *Connection) ExpectFrame(layers Layers, timeout time.Duration) (Layers, error) {
	deadline := time.Now().Add(timeout)
	errs := multierr.Combine()
	for {
		var gotLayers Layers
		if timeout = time.Until(deadline); timeout > 0 {
			gotLayers = conn.recvFrame(timeout)
		}
		if gotLayers == nil {
			if errs == nil {
				return nil, fmt.Errorf("got no frames")
			}
			return nil, errs
		}
		toMatch := conn.incoming(gotLayers)
		if toMatch == nil {
			continue // Not enough layers in gotLayers for matching.
		}
		if err := toMatch.merge(layers); err != nil {
			continue // Failing to merge is not matching.
		}
		if toMatch.match(gotLayers) {
			for i, s := range conn.layerStates {
				if err := s.received(gotLayers[i]); err != nil {
					conn.t.Fatal(err)
				}
			}
			return gotLayers, nil
		}
		errs = multierr.Combine(errs, LayersError{got: gotLayers, want: toMatch})
	}
}

// TCPIPv4 maintains the state for all the layers in a TCP/IPv4 connection.
type TCPIPv4 Connection

// NewTCPIPv4 creates a new TCPIPv4 connection with reasonable defaults.
func NewTCPIPv4(t *testing.T, outgoingTCP, incomingTCP TCP) TCPIPv4 {
	etherState, err := newEtherState(Ether{}, Ether{})
	if err != nil {
		t.Fatalf("can't make etherState: %s", err)
	}
	ipv4State, err := newIPv4State(IPv4{}, IPv4{})
	if err != nil {
		t.Fatalf("can't make ipv4State: %s", err)
	}
	tcpState, err := newTCPState(outgoingTCP, incomingTCP)
	if err != nil {
		t.Fatalf("can't make tcpState: %s", err)
	}
	injector, err := NewInjector(t)
	if err != nil {
		t.Fatalf("can't make injector: %s", err)
	}
	sniffer, err := NewSniffer(t)
	if err != nil {
		t.Fatalf("can't make sniffer: %s", err)
	}

	return TCPIPv4{
		layerStates: []layerState{etherState, ipv4State, tcpState},
		injector:    injector,
		sniffer:     sniffer,
		t:           t,
	}
}

// Handshake performs a TCP 3-way handshake. The input Connection should have a
// final TCP Layer.
func (conn *TCPIPv4) Handshake() {
	// Send the SYN.
	conn.Send(TCP{Flags: Uint8(header.TCPFlagSyn)})

	// Wait for the SYN-ACK.
	synAck, err := conn.Expect(TCP{Flags: Uint8(header.TCPFlagSyn | header.TCPFlagAck)}, time.Second)
	if synAck == nil {
		conn.t.Fatalf("didn't get synack during handshake: %s", err)
	}
	conn.layerStates[len(conn.layerStates)-1].(*tcpState).synAck = synAck

	// Send an ACK.
	conn.Send(TCP{Flags: Uint8(header.TCPFlagAck)})
}

// ExpectData is a convenient method that expects a Layer and the Layer after
// it. If it doens't arrive in time, it returns nil.
func (conn *TCPIPv4) ExpectData(tcp *TCP, payload *Payload, timeout time.Duration) (Layers, error) {
	expected := make([]Layer, len(conn.layerStates))
	expected[len(expected)-1] = tcp
	if payload != nil {
		expected = append(expected, payload)
	}
	return (*Connection)(conn).ExpectFrame(expected, timeout)
}

// Send a packet with reasonable defaults. Potentially override the TCP layer in
// the connection with the provided layer and add additionLayers.
func (conn *TCPIPv4) Send(tcp TCP, additionalLayers ...Layer) {
	(*Connection)(conn).Send(&tcp, additionalLayers...)
}

// Close to clean up any resources held.
func (conn *TCPIPv4) Close() {
	(*Connection)(conn).Close()
}

// Expect a frame with the TCP layer matching the provided TCP within the
// timeout specified. If it doesn't arrive in time, it returns nil.
func (conn *TCPIPv4) Expect(tcp TCP, timeout time.Duration) (*TCP, error) {
	layer, err := (*Connection)(conn).Expect(&tcp, timeout)
	if layer == nil {
		return nil, err
	}
	gotTCP, ok := layer.(*TCP)
	if !ok {
		conn.t.Fatalf("expected %s to be TCP", layer)
	}
	return gotTCP, err
}

// RemoteSeqNum returns the next expected sequence number from the DUT.
func (conn *TCPIPv4) RemoteSeqNum() *seqnum.Value {
	state, ok := conn.layerStates[len(conn.layerStates)-1].(*tcpState)
	if !ok {
		conn.t.Fatalf("expected final state of %v to be tcpState", conn.layerStates)
	}
	return state.remoteSeqNum
}

// NewUDPIPv4 creates a new UDPIPv4 connection with reasonable defaults.
func NewUDPIPv4(t *testing.T, outgoingUDP, incomingUDP UDP) Connection {
	etherState, err := newEtherState(Ether{}, Ether{})
	if err != nil {
		t.Fatalf("can't make etherState: %s", err)
	}
	ipv4State, err := newIPv4State(IPv4{}, IPv4{})
	if err != nil {
		t.Fatalf("can't make ipv4State: %s", err)
	}
	tcpState, err := newUDPState(outgoingUDP, incomingUDP)
	if err != nil {
		t.Fatalf("can't make udpState: %s", err)
	}
	injector, err := NewInjector(t)
	if err != nil {
		t.Fatalf("can't make injector: %s", err)
	}
	sniffer, err := NewSniffer(t)
	if err != nil {
		t.Fatalf("can't make sniffer: %s", err)
	}

	return Connection{
		layerStates: []layerState{etherState, ipv4State, tcpState},
		injector:    injector,
		sniffer:     sniffer,
		t:           t,
	}
}
