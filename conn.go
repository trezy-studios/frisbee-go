/*
	Copyright 2021 Loophole Labs

	Licensed under the Apache License, Version 2.0 (the "License");
	you may not use this file except in compliance with the License.
	You may obtain a copy of the License at

		   http://www.apache.org/licenses/LICENSE-2.0

	Unless required by applicable law or agreed to in writing, software
	distributed under the License is distributed on an "AS IS" BASIS,
	WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
	See the License for the specific language governing permissions and
	limitations under the License.
*/

package frisbee

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"github.com/loophole-labs/frisbee/internal/errors"
	"github.com/loophole-labs/frisbee/internal/protocol"
	"github.com/loophole-labs/frisbee/internal/ringbuffer"
	"github.com/rs/zerolog"
	"go.uber.org/atomic"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// These are states that frisbee connections can be in:
const (
	// CONNECTED is used to specify that the connection is functioning normally
	CONNECTED = int32(iota)

	// CLOSED is used to specify that the connection has been closed (possibly due to an error)
	CLOSED

	// PAUSED is used in the event of a read or write error and puts the connection into a paused state,
	// this is then used by the reconnection logic to resume the connection
	PAUSED
)

var (
	defaultLogger = zerolog.New(os.Stdout)
)

const DefaultBufferSize = 1 << 19

type incomingBuffer struct {
	sync.Mutex
	buffer *bytes.Buffer
}

func newIncomingBuffer() *incomingBuffer {
	return &incomingBuffer{
		buffer: bytes.NewBuffer(make([]byte, 0, DefaultBufferSize)),
	}
}

// Conn is the underlying frisbee connection which has extremely efficient read and write logic and
// can handle the specific frisbee requirements. This is not meant to be used on its own, and instead is
// meant to be used by frisbee client and server implementations
type Conn struct {
	sync.Mutex
	conn             net.Conn
	state            *atomic.Int32
	writer           *bufio.Writer
	flusher          chan struct{}
	incomingMessages *ringbuffer.RingBuffer
	streamConnMutex  sync.RWMutex
	streamConns      map[uint64]*StreamConn
	StreamConnCh     chan *StreamConn
	logger           *zerolog.Logger
	wg               sync.WaitGroup
	error            *atomic.Error
}

// Connect creates a new TCP connection (using net.Dial) and warps it in a frisbee connection
func Connect(network string, addr string, keepAlive time.Duration, logger *zerolog.Logger, TLSConfig *tls.Config) (*Conn, error) {
	var conn net.Conn
	var err error

	if TLSConfig != nil {
		conn, err = tls.Dial(network, addr, TLSConfig)
	} else {
		conn, err = net.Dial(network, addr)
	}

	if err != nil {
		return nil, errors.WithContext(err, DIAL)
	}
	_ = conn.(*net.TCPConn).SetKeepAlive(true)
	_ = conn.(*net.TCPConn).SetKeepAlivePeriod(keepAlive)

	return New(conn, logger), nil
}

// New takes an existing net.Conn object and wraps it in a frisbee connection
func New(c net.Conn, logger *zerolog.Logger) (conn *Conn) {
	conn = &Conn{
		conn:             c,
		state:            atomic.NewInt32(CONNECTED),
		writer:           bufio.NewWriterSize(c, DefaultBufferSize),
		incomingMessages: ringbuffer.NewRingBuffer(DefaultBufferSize),
		streamConns:      make(map[uint64]*StreamConn),
		StreamConnCh:     make(chan *StreamConn, 1024),
		flusher:          make(chan struct{}, 1024),
		logger:           logger,
		error:            atomic.NewError(ConnectionClosed),
	}

	if logger == nil {
		conn.logger = &defaultLogger
	}

	conn.wg.Add(2)
	go conn.flushLoop()
	go conn.readLoop()

	return
}

func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

// LocalAddr returns the local address of the underlying net.Conn
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the remote address of the underlying net.Conn
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// WriteMessage takes a frisbee.Message and some (optional) accompanying content, and queues it up to send asynchronously.
//
// If message.ContentLength == 0, then the content array must be nil. Otherwise, it is required that message.ContentLength == len(content).
func (c *Conn) WriteMessage(message *Message, content *[]byte) error {
	if content != nil && int(message.ContentLength) != len(*content) {
		return InvalidContentLength
	}

	var encodedMessage [protocol.MessageSize]byte
	copy(encodedMessage[protocol.ReservedOffset:protocol.ReservedOffset+protocol.ReservedSize], protocol.ReservedBytes)
	binary.BigEndian.PutUint32(encodedMessage[protocol.FromOffset:protocol.FromOffset+protocol.FromSize], message.From)
	binary.BigEndian.PutUint32(encodedMessage[protocol.ToOffset:protocol.ToOffset+protocol.ToSize], message.To)
	binary.BigEndian.PutUint64(encodedMessage[protocol.IdOffset:protocol.IdOffset+protocol.IdSize], message.Id)
	binary.BigEndian.PutUint32(encodedMessage[protocol.OperationOffset:protocol.OperationOffset+protocol.OperationSize], message.Operation)
	binary.BigEndian.PutUint64(encodedMessage[protocol.ContentLengthOffset:protocol.ContentLengthOffset+protocol.ContentLengthSize], message.ContentLength)

	c.Lock()
	if c.state.Load() != CONNECTED {
		c.Unlock()
		return c.Error()
	}

	_, err := c.writer.Write(encodedMessage[:])
	if err != nil {
		c.Unlock()
		if c.state.Load() != CONNECTED {
			err = c.Error()
			c.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
			return errors.WithContext(err, WRITE)
		}
		c.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
		return c.closeWithError(err)
	}
	if content != nil {
		_, err = c.writer.Write(*content)
		if err != nil {
			c.Unlock()
			if c.state.Load() != CONNECTED {
				err = c.Error()
				c.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
				return errors.WithContext(err, WRITE)
			}
			c.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
			return c.closeWithError(err)
		}
	}

	if len(c.flusher) == 0 {
		select {
		case c.flusher <- struct{}{}:
		default:
		}
	}

	c.Unlock()

	return nil
}

// ReadMessage is a blocking function that will wait until a frisbee message is available and then return it (and its content).
// In the event that the connection is closed, ReadMessage will return an error.
func (c *Conn) ReadMessage() (*Message, *[]byte, error) {
	if c.state.Load() != CONNECTED {
		return nil, nil, c.Error()
	}

	readPacket, err := c.incomingMessages.Pop()
	if err != nil {
		if c.state.Load() != CONNECTED {
			err = c.Error()
			c.logger.Error().Msgf(errors.WithContext(err, POP).Error())
			return nil, nil, errors.WithContext(err, POP)
		}
		c.logger.Error().Msgf(errors.WithContext(err, POP).Error())
		return nil, nil, errors.WithContext(c.closeWithError(err), POP)
	}

	return (*Message)(readPacket.Message), readPacket.Content, nil
}

// Flush allows for synchronous messaging by flushing the message buffer and instantly sending messages
func (c *Conn) Flush() error {
	c.Lock()
	if c.writer.Buffered() > 0 {
		err := c.writer.Flush()
		if err != nil {
			c.Unlock()
			_ = c.closeWithError(err)
			return err
		}
	}
	c.Unlock()
	return nil
}

// WriteBufferSize returns the size of the underlying message buffer (used for internal message handling and for heartbeat logic)
func (c *Conn) WriteBufferSize() int {
	c.Lock()
	if c.state.Load() != CONNECTED {
		c.Unlock()
		return 0
	}
	i := c.writer.Buffered()
	c.Unlock()
	return i
}

// Logger returns the underlying logger of the frisbee connection
func (c *Conn) Logger() *zerolog.Logger {
	return c.logger
}

// Error returns the error that caused the frisbee.Conn to close or go into a paused state
func (c *Conn) Error() error {
	return c.error.Load()
}

// Raw shuts off all of frisbee's underlying functionality and converts the frisbee connection into a normal TCP connection (net.Conn)
func (c *Conn) Raw() net.Conn {
	_ = c.close()
	return c.conn
}

// Close closes the frisbee connection gracefully
func (c *Conn) Close() error {
	err := c.close()
	if errors.Is(err, ConnectionClosed) {
		return nil
	}
	_ = c.conn.Close()
	return err
}

func (c *Conn) killGoroutines() {
	c.Lock()
	c.incomingMessages.Close()
	close(c.flusher)
	c.Unlock()
	_ = c.conn.SetReadDeadline(time.Now())
	c.wg.Wait()
	_ = c.conn.SetReadDeadline(time.Time{})
}

func (c *Conn) pause() error {
	if c.state.CAS(CONNECTED, PAUSED) {
		c.error.Store(ConnectionPaused)
		c.killGoroutines()
		return nil
	} else if c.state.Load() == PAUSED {
		return ConnectionPaused
	}
	return ConnectionNotInitialized
}

func (c *Conn) close() error {
	if c.state.CAS(CONNECTED, CLOSED) {
		c.error.Store(ConnectionClosed)
		c.killGoroutines()
		c.Lock()
		if c.writer.Buffered() > 0 {
			_ = c.writer.Flush()
		}
		c.Unlock()
		return nil
	} else if c.state.CAS(PAUSED, CLOSED) {
		c.error.Store(ConnectionClosed)
		return nil
	}
	return ConnectionClosed
}

func (c *Conn) closeWithError(err error) error {
	if os.IsTimeout(err) {
		return err
	} else if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		pauseError := c.pause()
		if errors.Is(pauseError, ConnectionNotInitialized) {
			c.Logger().Debug().Msgf("attempted to close connection with error, but connection not initialized (inner error: %+v)", err)
			return ConnectionNotInitialized
		} else {
			c.Logger().Debug().Msgf("attempted to close connection with error, but error was EOF so pausing connection instead (inner error: %+v)", err)
			return ConnectionPaused
		}
	} else {
		closeError := c.close()
		if errors.Is(closeError, ConnectionClosed) {
			c.Logger().Debug().Msgf("attempted to close connection with error, but connection already closed (inner error: %+v)", err)
			return ConnectionClosed
		} else {
			c.Logger().Debug().Msgf("closing connection with error: %+v", err)
		}
	}
	c.error.Store(err)
	_ = c.conn.Close()
	return err
}

func (c *Conn) flushLoop() {
	defer c.wg.Done()
	for {
		if _, ok := <-c.flusher; !ok {
			return
		}
		c.Lock()
		if c.writer.Buffered() > 0 {
			err := c.writer.Flush()
			if err != nil {
				c.Unlock()
				_ = c.closeWithError(err)
				return
			}
		}
		c.Unlock()
	}
}

func (c *Conn) readLoop() {
	defer c.wg.Done()
	buf := make([]byte, DefaultBufferSize)
	var index int
	for {
		buf = buf[:cap(buf)]
		if len(buf) < protocol.MessageSize {
			_ = c.closeWithError(InvalidBufferLength)
			return
		}
		var n int
		var err error
		for n < protocol.MessageSize {
			var nn int
			nn, err = c.conn.Read(buf[n:])
			n += nn
			if err != nil {
				if n < protocol.MessageSize {
					_ = c.closeWithError(err)
					return
				}
				break
			}
		}

		index = 0
		for index < n {

			if !bytes.Equal(buf[index+protocol.ReservedOffset:index+protocol.ReservedOffset+protocol.ReservedSize], protocol.ReservedBytes) {
				c.Logger().Error().Msgf(InvalidBufferContents.Error())
				break
			}

			decodedMessage := protocol.Message{
				From:          binary.BigEndian.Uint32(buf[index+protocol.FromOffset : index+protocol.FromOffset+protocol.FromSize]),
				To:            binary.BigEndian.Uint32(buf[index+protocol.ToOffset : index+protocol.ToOffset+protocol.ToSize]),
				Id:            binary.BigEndian.Uint64(buf[index+protocol.IdOffset : index+protocol.IdOffset+protocol.IdSize]),
				Operation:     binary.BigEndian.Uint32(buf[index+protocol.OperationOffset : index+protocol.OperationOffset+protocol.OperationSize]),
				ContentLength: binary.BigEndian.Uint64(buf[index+protocol.ContentLengthOffset : index+protocol.ContentLengthOffset+protocol.ContentLengthSize]),
			}

			index += protocol.MessageSize

			if decodedMessage.Operation == STREAMCLOSE {
				c.streamConnMutex.RLock()
				streamConn := c.streamConns[decodedMessage.Id]
				c.streamConnMutex.RUnlock()
				if streamConn != nil {
					streamConn.closed.Store(true)
					c.streamConnMutex.Lock()
					delete(c.streamConns, decodedMessage.Id)
					c.streamConnMutex.Unlock()
				}
				goto READ
			}
			if decodedMessage.ContentLength > 0 {
				switch decodedMessage.Operation {
				case STREAM:
					c.streamConnMutex.RLock()
					streamConn := c.streamConns[decodedMessage.Id]
					c.streamConnMutex.RUnlock()
					if streamConn == nil {
						streamConn = c.NewStreamConn(decodedMessage.Id)
						c.streamConnMutex.Lock()
						c.streamConns[decodedMessage.Id] = streamConn
						c.streamConnMutex.Unlock()
						select {
						case c.StreamConnCh <- streamConn:
						default:
						}
					}
					if n-index < int(decodedMessage.ContentLength) {
						streamConn.incomingBuffer.Lock()
						for streamConn.incomingBuffer.buffer.Cap()-streamConn.incomingBuffer.buffer.Len() < int(decodedMessage.ContentLength) {
							streamConn.incomingBuffer.buffer.Grow(1 << 19)
						}
						cp, err := streamConn.incomingBuffer.buffer.Write(buf[index:n])
						if err != nil {
							c.Logger().Debug().Msgf(errors.WithContext(err, WRITE).Error())
							_ = c.closeWithError(err)
							return
						}
						min := int64(int(decodedMessage.ContentLength) - cp)
						index = n
						_, err = io.CopyN(streamConn.incomingBuffer.buffer, c.conn, min)
						if err != nil {
							c.Logger().Debug().Msgf(errors.WithContext(err, WRITE).Error())
							_ = c.closeWithError(err)
							return
						}
						streamConn.incomingBuffer.Unlock()
					} else {
						streamConn.incomingBuffer.Lock()
						for streamConn.incomingBuffer.buffer.Cap()-streamConn.incomingBuffer.buffer.Len() < int(decodedMessage.ContentLength) {
							streamConn.incomingBuffer.buffer.Grow(1 << 19)
						}
						cp, err := streamConn.incomingBuffer.buffer.Write(buf[index : index+int(decodedMessage.ContentLength)])
						if err != nil {
							c.Logger().Debug().Msgf(errors.WithContext(err, WRITE).Error())
							_ = c.closeWithError(err)
							return
						}
						streamConn.incomingBuffer.Unlock()
						index += cp
					}
				default:
					readContent := make([]byte, decodedMessage.ContentLength)
					if n-index < int(decodedMessage.ContentLength) {
						for cap(buf) < int(decodedMessage.ContentLength) {
							buf = append(buf[:cap(buf)], 0)
							buf = buf[:cap(buf)]
						}
						cp := copy(readContent, buf[index:n])
						buf = buf[:cap(buf)]
						min := int(decodedMessage.ContentLength) - cp
						if len(buf) < min {
							_ = c.closeWithError(InvalidBufferLength)
							return
						}
						n = 0
						for n < min {
							var nn int
							nn, err = c.conn.Read(buf[n:])
							n += nn
							if err != nil {
								if n < min {
									_ = c.closeWithError(err)
									return
								}
								break
							}
						}
						copy(readContent[cp:], buf[:min])
						index = min
					} else {
						copy(readContent, buf[index:index+int(decodedMessage.ContentLength)])
						index += int(decodedMessage.ContentLength)
					}
					err = c.incomingMessages.Push(&protocol.Packet{
						Message: &decodedMessage,
						Content: &readContent,
					})
					if err != nil {
						c.Logger().Debug().Msgf(errors.WithContext(err, PUSH).Error())
						_ = c.closeWithError(err)
						return
					}
				}
			} else {
				err = c.incomingMessages.Push(&protocol.Packet{
					Message: &decodedMessage,
					Content: nil,
				})
				if err != nil {
					c.Logger().Debug().Msgf(errors.WithContext(err, PUSH).Error())
					_ = c.closeWithError(err)
					return
				}
			}
		READ:
			if n == index {
				index = 0
				buf = buf[:cap(buf)]
				if len(buf) < protocol.MessageSize {
					_ = c.closeWithError(InvalidBufferLength)
					break
				}
				n = 0
				for n < protocol.MessageSize {
					var nn int
					nn, err = c.conn.Read(buf[n:])
					n += nn
					if err != nil {
						if n < protocol.MessageSize {
							_ = c.closeWithError(err)
							return
						}
						break
					}
				}
			} else if n-index < protocol.MessageSize {
				copy(buf, buf[index:n])
				n -= index
				index = n

				buf = buf[:cap(buf)]
				min := protocol.MessageSize - index
				if len(buf) < min {
					_ = c.closeWithError(InvalidBufferLength)
					break
				}
				n = 0
				for n < min {
					var nn int
					nn, err = c.conn.Read(buf[index+n:])
					n += nn
					if err != nil {
						if n < min {
							_ = c.closeWithError(err)
							return
						}
						break
					}
				}
				n += index
				index = 0
			}
		}
	}
}

type StreamConn struct {
	*Conn
	id             uint64
	incomingBuffer *incomingBuffer
	closed         *atomic.Bool
}

func (c *Conn) NewStreamConn(id uint64) *StreamConn {
	streamConn := &StreamConn{
		Conn:           c,
		id:             id,
		incomingBuffer: newIncomingBuffer(),
		closed:         atomic.NewBool(false),
	}

	c.streamConnMutex.Lock()
	c.streamConns[id] = streamConn
	c.streamConnMutex.Unlock()

	return streamConn
}

func (s *StreamConn) ID() uint64 {
	return s.id
}

func (s *StreamConn) Closed() bool {
	return s.closed.Load()
}

func (s *StreamConn) Close() error {
	s.closed.Store(true)
	return s.WriteMessage(&Message{
		Id:            s.id,
		Operation:     STREAMCLOSE,
		ContentLength: 0,
	}, nil)
}

// Write takes a byte slice and sends a STREAM message
func (s *StreamConn) Write(p []byte) (int, error) {
	var encodedMessage [protocol.MessageSize]byte

	copy(encodedMessage[protocol.ReservedOffset:protocol.ReservedOffset+protocol.ReservedSize], protocol.ReservedBytes)
	binary.BigEndian.PutUint64(encodedMessage[protocol.IdOffset:protocol.IdOffset+protocol.IdSize], s.id)
	binary.BigEndian.PutUint32(encodedMessage[protocol.OperationOffset:protocol.OperationOffset+protocol.OperationSize], STREAM)
	binary.BigEndian.PutUint64(encodedMessage[protocol.ContentLengthOffset:protocol.ContentLengthOffset+protocol.ContentLengthSize], uint64(len(p)))

	s.Lock()
	if s.state.Load() != CONNECTED {
		s.Unlock()
		return 0, s.Error()
	}

	if s.Closed() {
		s.Unlock()
		return 0, ConnectionClosed
	}

	_, err := s.writer.Write(encodedMessage[:])
	if err != nil {
		s.Unlock()
		if s.state.Load() != CONNECTED {
			err = s.Error()
			s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
			return 0, errors.WithContext(err, WRITE)
		}
		s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
		return 0, s.closeWithError(err)
	}

	_, err = s.writer.Write(p)
	if err != nil {
		s.Unlock()
		if s.state.Load() != CONNECTED {
			err = s.Error()
			s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
			return 0, errors.WithContext(err, WRITE)
		}
		s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
		return 0, s.closeWithError(err)
	}

	if len(s.flusher) == 0 {
		select {
		case s.flusher <- struct{}{}:
		default:
		}
	}

	s.Unlock()
	return len(p), nil
}

// ReadFrom is a function that will send STREAM messages from an io.Reader until EOF or an error occurs
// In the event that the connection is closed, ReadFrom will return an error.
func (s *StreamConn) ReadFrom(r io.Reader) (n int64, err error) {
	buf := make([]byte, DefaultBufferSize)

	var encodedMessage [protocol.MessageSize]byte

	copy(encodedMessage[protocol.ReservedOffset:protocol.ReservedOffset+protocol.ReservedSize], protocol.ReservedBytes)
	binary.BigEndian.PutUint64(encodedMessage[protocol.IdOffset:protocol.IdOffset+protocol.IdSize], s.id)
	binary.BigEndian.PutUint32(encodedMessage[protocol.OperationOffset:protocol.OperationOffset+protocol.OperationSize], STREAM)

	for {
		var nn int
		if s.state.Load() != CONNECTED {
			return n, err
		}

		if s.Closed() {
			return n, ConnectionClosed
		}
		nn, err = r.Read(buf)
		if nn == 0 || err != nil {
			break
		}

		n += int64(nn)

		binary.BigEndian.PutUint64(encodedMessage[protocol.ContentLengthOffset:protocol.ContentLengthOffset+protocol.ContentLengthSize], uint64(nn))

		s.Lock()

		_, err := s.writer.Write(encodedMessage[:])
		if err != nil {
			s.Unlock()
			if s.state.Load() != CONNECTED {
				err = s.Error()
				s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
				return n, errors.WithContext(err, WRITE)
			}
			s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
			return n, s.closeWithError(err)
		}

		_, err = s.writer.Write(buf[:nn])
		if err != nil {
			s.Unlock()
			if s.state.Load() != CONNECTED {
				err = s.Error()
				s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
				return n, errors.WithContext(err, WRITE)
			}
			s.logger.Error().Msgf(errors.WithContext(err, WRITE).Error())
			return n, s.closeWithError(err)
		}

		if len(s.flusher) == 0 {
			select {
			case s.flusher <- struct{}{}:
			default:
			}
		}
		s.Unlock()
	}

	if errors.Is(err, io.EOF) {
		err = nil
	}

	return
}

// Read is a function that will read buffer messages into a byte slice.
// In the event that the connection is closed, Read will return an error.
func (s *StreamConn) Read(p []byte) (int, error) {
LOOP:
	s.incomingBuffer.Lock()
	if s.state.Load() != CONNECTED {
		s.incomingBuffer.Unlock()
		return 0, ConnectionClosed
	}
	if s.Closed() {
		s.incomingBuffer.Unlock()
		return 0, ConnectionClosed
	}
	for s.incomingBuffer.buffer.Len() == 0 {
		s.incomingBuffer.Unlock()
		goto LOOP
	}
	defer s.incomingBuffer.Unlock()
	return s.incomingBuffer.buffer.Read(p)
}

// WriteTo is a function that will read buffer messages into an io.Writer until EOF or an error occurs
// In the event that the connection is closed, WriteTo will return an error.
func (s *StreamConn) WriteTo(w io.Writer) (n int64, err error) {
	for err == nil {
		s.incomingBuffer.Lock()
		var nn int
		if s.state.Load() != CONNECTED {
			s.incomingBuffer.Unlock()
			return n, ConnectionClosed
		}
		if s.Closed() {
			s.incomingBuffer.Unlock()
			return n, ConnectionClosed
		}
		if s.incomingBuffer.buffer.Len() == 0 {
			s.incomingBuffer.Unlock()
			continue
		} else {
			nn, err = w.Write(s.incomingBuffer.buffer.Bytes())
			if nn > 0 {
				s.incomingBuffer.buffer.Next(nn)
				n += int64(nn)
			}
		}
		s.incomingBuffer.Unlock()
	}

	if errors.Is(err, io.EOF) {
		err = nil
	}
	return
}
