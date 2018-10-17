/*
Copyright 2017 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreedto in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysql

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"gopkg.in/src-d/go-vitess.v1/bucketpool"
	querypb "gopkg.in/src-d/go-vitess.v1/vt/proto/query"
)

const (
	// connBufferSize is how much we buffer for reading and
	// writing. It is also how much we allocate for ephemeral buffers.
	connBufferSize = 16 * 1024
)

// Constants for how ephemeral buffers were used for reading / writing.
const (
	// ephemeralUnused means the ephemeral buffer is not in use at this
	// moment. This is the default value, and is checked so we don't
	// read or write a packet while one is already used.
	ephemeralUnused = iota

	// ephemeralWriteSingleBuffer means a single buffer was
	// allocated to write a packet.  It is in
	// c.currentEphemeralWriteBuffer. The first four bytes contain size
	// and sequence.
	ephemeralWriteSingleBuffer

	// ephemeralWriteBigBuffer means a big buffer was allocated to
	// write a packet, and will need to be split when sending.
	// The allocated buffer is in c.currentEphemeralWriteBuffer.
	ephemeralWriteBigBuffer

	// ephemeralReadSingleBuffer means we are using a pool of buffers
	// for reading.
	ephemeralReadSingleBuffer

	// ephemeralReadBigBuffer means we allocated a very big buffer
	// and we can't reuse it at all.
	ephemeralReadBigBuffer
)

// A Getter has a Get()
type Getter interface {
	Get() *querypb.VTGateCallerID
}

// Conn is a connection between a client and a server, using the MySQL
// binary protocol. It is built on top of an existing net.Conn, that
// has already been established.
//
// Use Connect on the client side to create a connection.
// Use NewListener to create a server side and listen for connections.
type Conn struct {
	// conn is the underlying network connection.
	// Calling Close() on the Conn will close this connection.
	// If there are any ongoing reads or writes, they may get interrupted.
	conn net.Conn

	// ConnectionID is set:
	// - at Connect() time for clients, with the value returned by
	// the server.
	// - at accept time for the server.
	ConnectionID uint32

	// Closed is set to true when Close() is called on the connection.
	Closed bool

	// Capabilities is the current set of features this connection
	// is using.  It is the features that are both supported by
	// the client and the server, and currently in use.
	// It is set during the initial handshake.
	//
	// It is only used for CapabilityClientDeprecateEOF
	// and CapabilityClientFoundRows.
	Capabilities uint32

	// CharacterSet is the character set used by the other side of the
	// connection.
	// It is set during the initial handshake.
	// See the values in constants.go.
	CharacterSet uint8

	// User is the name used by the client to connect.
	// It is set during the initial handshake.
	User string

	// UserData is custom data returned by the AuthServer module.
	// It is set during the initial handshake.
	UserData Getter

	// SchemaName is the default database name to use. It is set
	// during handshake, and by ComInitDb packets. Both client and
	// servers maintain it.
	SchemaName string

	// ServerVersion is set during Connect with the server
	// version.  It is not changed afterwards. It is unused for
	// server-side connections.
	ServerVersion string

	// flavor contains the auto-detected flavor for this client
	// connection. It is unused for server-side connections.
	flavor flavor

	// StatusFlags are the status flags we will base our returned flags on.
	// This is a bit field, with values documented in constants.go.
	// An interesting value here would be ServerStatusAutocommit.
	// It is only used by the server. These flags can be changed
	// by Handler methods.
	StatusFlags uint16

	// ClientData is a place where an application can store any
	// connection-related data. Mostly used on the server side, to
	// avoid maps indexed by ConnectionID for instance.
	ClientData interface{}

	// Packet encoding variables.
	bufferedReader *bufio.Reader
	bufferedWriter *bufio.Writer
	sequence       uint8

	// fields contains the fields definitions for an on-going
	// streaming query. It is set by ExecuteStreamFetch, and
	// cleared by the last FetchNext().  It is nil if no streaming
	// query is in progress.  If the streaming query returned no
	// fields, this is set to an empty array (but not nil).
	fields []*querypb.Field

	// Keep track of how and of the buffer we allocated for an
	// ephemeral packet on the read and write sides.
	// These fields are used by:
	// - startEphemeralPacket / writeEphemeralPacket methods for writes.
	// - readEphemeralPacket / recycleReadPacket methods for reads.
	currentEphemeralPolicy int
	// TODO (danieltahara): Ultimately get rid of this delineation.
	// currentEphemeralWriteBuffer and currentEphemeralReadBuffer used for tracking
	// allocated temporary buffers for writes and reads respectively.
	currentEphemeralWriteBuffer *[]byte
	currentEphemeralReadBuffer  *[]byte
}

// bufPool is used to allocate and free buffers in an efficient way.
var bufPool = bucketpool.New(connBufferSize, MaxPacketSize)

// writersPool is used for pooling bufio.Writer objects.
var writersPool = sync.Pool{New: func() interface{} { return bufio.NewWriterSize(nil, connBufferSize) }}

// newConn is an internal method to create a Conn. Used by client and server
// side for common creation code.
func newConn(conn net.Conn) *Conn {
	return &Conn{
		conn:           conn,
		bufferedReader: bufio.NewReaderSize(conn, connBufferSize),
	}
}

// newServerConn should be used to create server connections.
// The only difference from "client" newConn is ability to control buffer size
// for reads.
func newServerConn(conn net.Conn, connReadBufferSize int) *Conn {
	c := &Conn{
		conn: conn,
	}
	if connReadBufferSize > 0 {
		c.bufferedReader = bufio.NewReaderSize(conn, connReadBufferSize)
	}
	return c
}

// startWriterBuffering starts using buffered writes. This should
// be terminated by a call to flush.
func (c *Conn) startWriterBuffering() {
	c.bufferedWriter = writersPool.Get().(*bufio.Writer)
	c.bufferedWriter.Reset(c.conn)
}

// flush flushes the written data to the socket.
// This must be called to terminate startBuffering.
func (c *Conn) flush() error {
	if c.bufferedWriter == nil {
		return nil
	}

	defer func() {
		c.bufferedWriter.Reset(nil)
		writersPool.Put(c.bufferedWriter)
		c.bufferedWriter = nil
	}()

	return c.bufferedWriter.Flush()
}

// getWriter returns the current writer. It may be either
// the original connection or a wrapper.
func (c *Conn) getWriter() io.Writer {
	if c.bufferedWriter != nil {
		return c.bufferedWriter
	}
	return c.conn
}

// getReader returns reader for connection. It can be *bufio.Reader or net.Conn
// depending on which buffer size was passed to newServerConn.
func (c *Conn) getReader() io.Reader {
	if c.bufferedReader != nil {
		return c.bufferedReader
	}
	return c.conn
}

func (c *Conn) readHeaderFrom(r io.Reader) (int, error) {
	var header [4]byte
	// Note io.ReadFull will return two different types of errors:
	// 1. if the socket is already closed, and the go runtime knows it,
	//   then ReadFull will return an error (different than EOF),
	//   someting like 'read: connection reset by peer'.
	// 2. if the socket is not closed while we start the read,
	//   but gets closed after the read is started, we'll get io.EOF.
	if _, err := io.ReadFull(r, header[:]); err != nil {
		// The special casing of propagating io.EOF up
		// is used by the server side only, to suppress an error
		// message if a client just disconnects.
		if err == io.EOF {
			return 0, err
		}
		if strings.HasSuffix(err.Error(), "read: connection reset by peer") {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("io.ReadFull(header size) failed: %v", err)
	}

	sequence := uint8(header[3])
	if sequence != c.sequence {
		return 0, fmt.Errorf("invalid sequence, expected %v got %v", c.sequence, sequence)
	}

	c.sequence++

	return int(uint32(header[0]) | uint32(header[1])<<8 | uint32(header[2])<<16), nil
}

// readEphemeralPacket attempts to read a packet into buffer from sync.Pool.  Do
// not use this method if the contents of the packet needs to be kept
// after the next readEphemeralPacket.
//
// Note if the connection is closed already, an error will be
// returned, and it may not be io.EOF. If the connection closes while
// we are stuck waiting for data, an error will also be returned, and
// it most likely will be io.EOF.
func (c *Conn) readEphemeralPacket() ([]byte, error) {
	if c.currentEphemeralPolicy != ephemeralUnused {
		panic(fmt.Errorf("readEphemeralPacket: unexpected currentEphemeralPolicy: %v", c.currentEphemeralPolicy))
	}

	r := c.getReader()

	length, err := c.readHeaderFrom(r)
	if err != nil {
		return nil, err
	}

	if length == 0 {
		// This can be caused by the packet after a packet of
		// exactly size MaxPacketSize.
		return nil, nil
	}

	// Use the bufPool.
	if length < MaxPacketSize {
		c.currentEphemeralPolicy = ephemeralReadSingleBuffer
		c.currentEphemeralReadBuffer = bufPool.Get(length)
		if _, err := io.ReadFull(r, *c.currentEphemeralReadBuffer); err != nil {
			return nil, fmt.Errorf("io.ReadFull(packet body of length %v) failed: %v", length, err)
		}
		return *c.currentEphemeralReadBuffer, nil
	}

	// Much slower path, revert to allocating everything from scratch.
	// We're going to concatenate a lot of data anyway, can't really
	// optimize this code path easily.
	c.currentEphemeralPolicy = ephemeralReadBigBuffer
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("io.ReadFull(packet body of length %v) failed: %v", length, err)
	}
	for {
		next, err := c.readOnePacket()
		if err != nil {
			return nil, err
		}

		if len(next) == 0 {
			// Again, the packet after a packet of exactly size MaxPacketSize.
			break
		}

		data = append(data, next...)
		if len(next) < MaxPacketSize {
			break
		}
	}

	return data, nil
}

// readEphemeralPacketDirect attempts to read a packet from the socket directly.
// It needs to be used for the first handshake packet the server receives,
// so we do't buffer the SSL negotiation packet. As a shortcut, only
// packets smaller than MaxPacketSize can be read here.
// This function usually shouldn't be used - use readEphemeralPacket.
func (c *Conn) readEphemeralPacketDirect() ([]byte, error) {
	if c.currentEphemeralPolicy != ephemeralUnused {
		panic(fmt.Errorf("readEphemeralPacketDirect: unexpected currentEphemeralPolicy: %v", c.currentEphemeralPolicy))
	}

	var r io.Reader = c.conn

	length, err := c.readHeaderFrom(r)
	if err != nil {
		return nil, err
	}

	if length == 0 {
		// This can be caused by the packet after a packet of
		// exactly size MaxPacketSize.
		return nil, nil
	}

	if length < MaxPacketSize {
		c.currentEphemeralPolicy = ephemeralReadSingleBuffer
		c.currentEphemeralReadBuffer = bufPool.Get(length)
		if _, err := io.ReadFull(r, *c.currentEphemeralReadBuffer); err != nil {
			return nil, fmt.Errorf("io.ReadFull(packet body of length %v) failed: %v", length, err)
		}
		return *c.currentEphemeralReadBuffer, nil
	}

	return nil, fmt.Errorf("readEphemeralPacketDirect doesn't support more than one packet")
}

// recycleReadPacket recycles the read packet. It needs to be called
// after readEphemeralPacket was called.
func (c *Conn) recycleReadPacket() {
	switch c.currentEphemeralPolicy {
	case ephemeralReadSingleBuffer:
		// We are using the pool, put the buffer back in.
		bufPool.Put(c.currentEphemeralReadBuffer)
		c.currentEphemeralReadBuffer = nil
	case ephemeralReadBigBuffer:
		// We allocated a one-time buffer we can't re-use.
		// Nothing to do. Nil out for safety.
		c.currentEphemeralReadBuffer = nil
	case ephemeralUnused, ephemeralWriteSingleBuffer, ephemeralWriteBigBuffer:
		// Programming error.
		panic(fmt.Errorf("trying to call recycleReadPacket while currentEphemeralPolicy is %d", c.currentEphemeralPolicy))
	}
	c.currentEphemeralPolicy = ephemeralUnused
}

// readOnePacket reads a single packet into a newly allocated buffer.
func (c *Conn) readOnePacket() ([]byte, error) {
	r := c.getReader()
	length, err := c.readHeaderFrom(r)
	if err != nil {
		return nil, err
	}
	if length == 0 {
		// This can be caused by the packet after a packet of
		// exactly size MaxPacketSize.
		return nil, nil
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("io.ReadFull(packet body of length %v) failed: %v", length, err)
	}
	return data, nil
}

// readPacket reads a packet from the underlying connection.
// It re-assembles packets that span more than one message.
// This method returns a generic error, not a SQLError.
func (c *Conn) readPacket() ([]byte, error) {
	// Optimize for a single packet case.
	data, err := c.readOnePacket()
	if err != nil {
		return nil, err
	}

	// This is a single packet.
	if len(data) < MaxPacketSize {
		return data, nil
	}

	// There is more than one packet, read them all.
	for {
		next, err := c.readOnePacket()
		if err != nil {
			return nil, err
		}

		if len(next) == 0 {
			// Again, the packet after a packet of exactly size MaxPacketSize.
			break
		}

		data = append(data, next...)
		if len(next) < MaxPacketSize {
			break
		}
	}

	return data, nil
}

// ReadPacket reads a packet from the underlying connection.
// it is the public API version, that returns a SQLError.
// The memory for the packet is always allocated, and it is owned by the caller
// after this function returns.
func (c *Conn) ReadPacket() ([]byte, error) {
	result, err := c.readPacket()
	if err != nil {
		return nil, NewSQLError(CRServerLost, SSUnknownSQLState, "%v", err)
	}
	return result, err
}

// writePacket writes a packet, possibly cutting it into multiple
// chunks.  Note this is not very efficient, as the client probably
// has to build the []byte and that makes a memory copy.
// Try to use startEphemeralPacket/writeEphemeralPacket instead.
//
// This method returns a generic error, not a SQLError.
func (c *Conn) writePacket(data []byte) error {
	index := 0
	length := len(data)

	w := c.getWriter()

	for {
		// Packet length is capped to MaxPacketSize.
		packetLength := length
		if packetLength > MaxPacketSize {
			packetLength = MaxPacketSize
		}

		// Compute and write the header.
		var header [4]byte
		header[0] = byte(packetLength)
		header[1] = byte(packetLength >> 8)
		header[2] = byte(packetLength >> 16)
		header[3] = c.sequence
		if n, err := w.Write(header[:]); err != nil {
			return fmt.Errorf("Write(header) failed: %v", err)
		} else if n != 4 {
			return fmt.Errorf("Write(header) returned a short write: %v < 4", n)
		}

		// Write the body.
		if n, err := w.Write(data[index : index+packetLength]); err != nil {
			return fmt.Errorf("Write(packet) failed: %v", err)
		} else if n != packetLength {
			return fmt.Errorf("Write(packet) returned a short write: %v < %v", n, packetLength)
		}

		// Update our state.
		c.sequence++
		length -= packetLength
		if length == 0 {
			if packetLength == MaxPacketSize {
				// The packet we just sent had exactly
				// MaxPacketSize size, we need to
				// sent a zero-size packet too.
				header[0] = 0
				header[1] = 0
				header[2] = 0
				header[3] = c.sequence
				if n, err := w.Write(header[:]); err != nil {
					return fmt.Errorf("Write(empty header) failed: %v", err)
				} else if n != 4 {
					return fmt.Errorf("Write(empty header) returned a short write: %v < 4", n)
				}
				c.sequence++
			}
			return nil
		}
		index += packetLength
	}
}

func (c *Conn) startEphemeralPacket(length int) []byte {
	if c.currentEphemeralPolicy != ephemeralUnused {
		panic("startEphemeralPacket cannot be used while a packet is already started.")
	}

	// get buffer from pool
	if length < MaxPacketSize {
		c.currentEphemeralPolicy = ephemeralWriteSingleBuffer

		c.currentEphemeralWriteBuffer = bufPool.Get(length + 4)
		(*c.currentEphemeralWriteBuffer)[0] = byte(length)
		(*c.currentEphemeralWriteBuffer)[1] = byte(length >> 8)
		(*c.currentEphemeralWriteBuffer)[2] = byte(length >> 16)
		(*c.currentEphemeralWriteBuffer)[3] = c.sequence
		c.sequence++
		return (*c.currentEphemeralWriteBuffer)[4:]
	}

	// Even slower path: create a full size buffer and return it.
	c.currentEphemeralPolicy = ephemeralWriteBigBuffer
	data := make([]byte, length)
	c.currentEphemeralWriteBuffer = &data
	return *c.currentEphemeralWriteBuffer
}

// writeEphemeralPacket writes the packet that was allocated by
// startEphemeralPacket.
func (c *Conn) writeEphemeralPacket() error {
	defer c.recycleWritePacket()

	switch c.currentEphemeralPolicy {
	case ephemeralWriteSingleBuffer:
		// Write the allocated buffer as a single buffer.
		// It has both header and data.
		if n, err := c.getWriter().Write(*c.currentEphemeralWriteBuffer); err != nil {
			return fmt.Errorf("Conn %v: Write(*c.currentEphemeralWriteBuffer) failed: %v", c.ID(), err)
		} else if n != len(*c.currentEphemeralWriteBuffer) {
			return fmt.Errorf("Conn %v: Write(*c.currentEphemeralWriteBuffer) returned a short write: %v < %v", c.ID(), n, len(*c.currentEphemeralWriteBuffer))
		}
	case ephemeralWriteBigBuffer:
		// This is the slower path for big data.
		if err := c.writePacket(*c.currentEphemeralWriteBuffer); err != nil {
			return fmt.Errorf("Conn %v: %v", c.ID(), err)
		}
	case ephemeralUnused, ephemeralReadSingleBuffer, ephemeralReadBigBuffer:
		// Programming error.
		panic(fmt.Errorf("Conn %v: trying to call writeEphemeralPacket while currentEphemeralPolicy is %v", c.ID(), c.currentEphemeralPolicy))
	}

	return nil
}

// recycleWritePacket recycles the write packet. It needs to be called
// after writeEphemeralPacket was called.
func (c *Conn) recycleWritePacket() {
	switch c.currentEphemeralPolicy {
	case ephemeralWriteSingleBuffer:
		// Release our reference so the buffer can be gced
		bufPool.Put(c.currentEphemeralWriteBuffer)
		c.currentEphemeralWriteBuffer = nil
	case ephemeralWriteBigBuffer:
		// We allocated a one-time buffer we can't re-use.
		// N.B. Unlike the read packet, we actually assign the big buffer to currentEphemeralReadBuffer,
		// so we should remove our reference to it.
		c.currentEphemeralWriteBuffer = nil
	case ephemeralUnused, ephemeralReadSingleBuffer, ephemeralReadBigBuffer:
		// Programming error.
		panic(fmt.Errorf("trying to call recycleWritePacket while currentEphemeralPolicy is %d", c.currentEphemeralPolicy))
	}
	c.currentEphemeralPolicy = ephemeralUnused
}

// writeComQuit writes a Quit message for the server, to indicate we
// want to close the connection.
// Client -> Server.
// Returns SQLError(CRServerGone) if it can't.
func (c *Conn) writeComQuit() error {
	// This is a new command, need to reset the sequence.
	c.sequence = 0

	data := c.startEphemeralPacket(1)
	data[0] = ComQuit
	if err := c.writeEphemeralPacket(); err != nil {
		return NewSQLError(CRServerGone, SSUnknownSQLState, err.Error())
	}
	return nil
}

// RemoteAddr returns the underlying socket RemoteAddr().
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// ID returns the MySQL connection ID for this connection.
func (c *Conn) ID() int64 {
	return int64(c.ConnectionID)
}

// Ident returns a useful identification string for error logging
func (c *Conn) String() string {
	return fmt.Sprintf("client %v (%s)", c.ConnectionID, c.RemoteAddr().String())
}

// Close closes the connection. It can be called from a different go
// routine to interrupt the current connection.
func (c *Conn) Close() {
	c.Closed = true
	c.conn.Close()
}

// IsClosed returns true if this connection was ever closed by the
// Close() method.  Note if the other side closes the connection, but
// Close() wasn't called, this will return false.
func (c *Conn) IsClosed() bool {
	return c.Closed
}

//
// Packet writing methods, for generic packets.
//

// writeOKPacket writes an OK packet.
// Server -> Client.
// This method returns a generic error, not a SQLError.
func (c *Conn) writeOKPacket(affectedRows, lastInsertID uint64, flags uint16, warnings uint16) error {
	length := 1 + // OKPacket
		lenEncIntSize(affectedRows) +
		lenEncIntSize(lastInsertID) +
		2 + // flags
		2 // warnings
	data := c.startEphemeralPacket(length)
	pos := 0
	pos = writeByte(data, pos, OKPacket)
	pos = writeLenEncInt(data, pos, affectedRows)
	pos = writeLenEncInt(data, pos, lastInsertID)
	pos = writeUint16(data, pos, flags)
	pos = writeUint16(data, pos, warnings)

	return c.writeEphemeralPacket()
}

// writeOKPacketWithEOFHeader writes an OK packet with an EOF header.
// This is used at the end of a result set if
// CapabilityClientDeprecateEOF is set.
// Server -> Client.
// This method returns a generic error, not a SQLError.
func (c *Conn) writeOKPacketWithEOFHeader(affectedRows, lastInsertID uint64, flags uint16, warnings uint16) error {
	length := 1 + // EOFPacket
		lenEncIntSize(affectedRows) +
		lenEncIntSize(lastInsertID) +
		2 + // flags
		2 // warnings
	data := c.startEphemeralPacket(length)
	pos := 0
	pos = writeByte(data, pos, EOFPacket)
	pos = writeLenEncInt(data, pos, affectedRows)
	pos = writeLenEncInt(data, pos, lastInsertID)
	pos = writeUint16(data, pos, flags)
	pos = writeUint16(data, pos, warnings)

	return c.writeEphemeralPacket()
}

// writeErrorPacket writes an error packet.
// Server -> Client.
// This method returns a generic error, not a SQLError.
func (c *Conn) writeErrorPacket(errorCode uint16, sqlState string, format string, args ...interface{}) error {
	errorMessage := fmt.Sprintf(format, args...)
	length := 1 + 2 + 1 + 5 + len(errorMessage)
	data := c.startEphemeralPacket(length)
	pos := 0
	pos = writeByte(data, pos, ErrPacket)
	pos = writeUint16(data, pos, errorCode)
	pos = writeByte(data, pos, '#')
	if sqlState == "" {
		sqlState = SSUnknownSQLState
	}
	if len(sqlState) != 5 {
		panic("sqlState has to be 5 characters long")
	}
	pos = writeEOFString(data, pos, sqlState)
	pos = writeEOFString(data, pos, errorMessage)

	return c.writeEphemeralPacket()
}

// writeErrorPacketFromError writes an error packet, from a regular error.
// See writeErrorPacket for other info.
func (c *Conn) writeErrorPacketFromError(err error) error {
	if se, ok := err.(*SQLError); ok {
		return c.writeErrorPacket(uint16(se.Num), se.State, "%v", se.Message)
	}

	return c.writeErrorPacket(ERUnknownError, SSUnknownSQLState, "unknown error: %v", err)
}

// writeEOFPacket writes an EOF packet, through the buffer, and
// doesn't flush (as it is used as part of a query result).
func (c *Conn) writeEOFPacket(flags uint16, warnings uint16) error {
	length := 5
	data := c.startEphemeralPacket(length)
	pos := 0
	pos = writeByte(data, pos, EOFPacket)
	pos = writeUint16(data, pos, warnings)
	pos = writeUint16(data, pos, flags)

	return c.writeEphemeralPacket()
}

//
// Packet parsing methods, for generic packets.
//

// isEOFPacket determines whether or not a data packet is a "true" EOF. DO NOT blindly compare the
// first byte of a packet to EOFPacket as you might do for other packet types, as 0xfe is overloaded
// as a first byte.
//
// Per https://dev.mysql.com/doc/internals/en/packet-EOF_Packet.html, a packet starting with 0xfe
// but having length >= 9 (on top of 4 byte header) is not a true EOF but a LengthEncodedInteger
// (typically preceding a LengthEncodedString). Thus, all EOF checks must validate the payload size
// before exiting.
//
// More specifically, an EOF packet can have 3 different lengths (1, 5, 7) depending on the client
// flags that are set. 7 comes from server versions of 5.7.5 or greater where ClientDeprecateEOF is
// set (i.e. uses an OK packet starting with 0xfe instead of 0x00 to signal EOF). Regardless, 8 is
// an upper bound otherwise it would be ambiguous w.r.t. LengthEncodedIntegers.
//
// More docs here:
// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_response_packets.html
func isEOFPacket(data []byte) bool {
	return data[0] == EOFPacket && len(data) < 9
}

// parseEOFPacket returns true if there are more results to receive.
func parseEOFPacket(data []byte) (bool, error) {
	// The status flag is in position 4 & 5
	statusFlags, _, ok := readUint16(data, 3)
	if !ok {
		return false, fmt.Errorf("invalid EOF packet statusFlags: %v", data)
	}
	return (statusFlags & ServerMoreResultsExists) != 0, nil
}

func parseOKPacket(data []byte) (uint64, uint64, uint16, uint16, error) {
	// We already read the type.
	pos := 1

	// Affected rows.
	affectedRows, pos, ok := readLenEncInt(data, pos)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("invalid OK packet affectedRows: %v", data)
	}

	// Last Insert ID.
	lastInsertID, pos, ok := readLenEncInt(data, pos)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("invalid OK packet lastInsertID: %v", data)
	}

	// Status flags.
	statusFlags, pos, ok := readUint16(data, pos)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("invalid OK packet statusFlags: %v", data)
	}

	// Warnings.
	warnings, pos, ok := readUint16(data, pos)
	if !ok {
		return 0, 0, 0, 0, fmt.Errorf("invalid OK packet warnings: %v", data)
	}

	return affectedRows, lastInsertID, statusFlags, warnings, nil
}

// isErrorPacket determines whether or not the packet is an error packet. Mostly here for
// consistency with isEOFPacket
func isErrorPacket(data []byte) bool {
	return data[0] == ErrPacket
}

// ParseErrorPacket parses the error packet and returns a SQLError.
func ParseErrorPacket(data []byte) error {
	// We already read the type.
	pos := 1

	// Error code is 2 bytes.
	code, pos, ok := readUint16(data, pos)
	if !ok {
		return NewSQLError(CRUnknownError, SSUnknownSQLState, "invalid error packet code: %v", data)
	}

	// '#' marker of the SQL state is 1 byte. Ignored.
	pos++

	// SQL state is 5 bytes
	sqlState, pos, ok := readBytes(data, pos, 5)
	if !ok {
		return NewSQLError(CRUnknownError, SSUnknownSQLState, "invalid error packet sqlState: %v", data)
	}

	// Human readable error message is the rest.
	msg := string(data[pos:])

	return NewSQLError(int(code), string(sqlState), "%v", msg)
}