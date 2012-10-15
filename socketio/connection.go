package socketio

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrDestroyed is used when the connection has been disconnected (i.e. can't be used anymore).
	ErrDestroyed = errors.New("connection is disconnected")

	// ErrQueueFull is used when the send queue is full.
	ErrQueueFull = errors.New("send queue is full")

	errMissingPostData = errors.New("Missing HTTP post data-field")
)

// Conn represents a single session and handles its handshaking,
// message buffering and reconnections.
type Conn struct {
	mutex            sync.Mutex
	socket           socket    // The i/o connection that abstract the transport.
	sio              *SocketIO // The server.
	sessionid        SessionID
	online           bool
	lastConnected    time.Time
	lastDisconnected time.Time
	lastMessage      time.Time
	lastHeartbeat    heartbeat
	numHeartbeats    int
	ticker           *DelayTimer
	queue            chan interface{} // Buffers the outgoing normal messages.
	serviceQueue     chan interface{} // Buffers the outgoing service messages.
	numConns         int              // Total number of reconnects.
	handshaked       bool             // Indicates if the handshake has been sent.
	disconnected     bool             // Indicates if the connection has been disconnected.
	wakeupFlusher    chan byte        // Used internally to wake up the flusher.
	wakeupReader     chan byte        // Used internally to wake up the reader.
	enc              Encoder
	dec              Decoder
	decBuf           bytes.Buffer
	raddr            string

	UserData interface{} // User data
}

// NewConn creates a new connection for the sio. It generates the session id and
// prepares the internal structure for usage.
func newConn(sio *SocketIO) (c *Conn, err error) {
	var sessionid SessionID
	if sessionid, err = NewSessionID(); err != nil {
		sio.Log("sio/newConn: newSessionID:", err)
		return
	}

	c = &Conn{
		sio:           sio,
		sessionid:     sessionid,
		wakeupFlusher: make(chan byte),
		wakeupReader:  make(chan byte),
		queue:         make(chan interface{}, sio.config.QueueLength),
		serviceQueue:  make(chan interface{}, 100), // TODO: Reasonable to expect this limit?
		enc:           sio.config.Codec.NewEncoder(),
		lastMessage:   time.Now(),
	}

	c.dec = sio.config.Codec.NewDecoder(&c.decBuf)

	return
}

// String returns a string representation of the connection and implements the
// fmt.Stringer interface.
func (c *Conn) String() string {
	return fmt.Sprintf("%v[%v]", c.sessionid, c.socket)
}

// SessionID return the session id of Conn
func (c *Conn) SessionID() SessionID {
	return c.sessionid
}

// RemoteAddr returns the remote network address of the connection in IP:port format
func (c *Conn) RemoteAddr() string {
	return c.raddr
}

// Send queues data for a delivery. It is totally content agnostic with one exception:
// the given data must be one of the following: a handshake, a heartbeat, an int, a string or
// it must be otherwise marshallable by the standard json package. If the send queue
// has reached sio.config.QueueLength or the connection has been disconnected,
// then the data is dropped and a an error is returned.
func (c *Conn) Send(data interface{}) (err error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.disconnected {
		return ErrDestroyed
	}

	switch data.(type) {
	case heartbeat:
		select {
		case c.serviceQueue <- data:
		default:
			return ErrQueueFull
		}
	default:
		select {
		case c.queue <- data:
		default:
			return ErrQueueFull
		}
	}

	return nil
}

func (c *Conn) Close() error {
	c.mutex.Lock()

	if c.disconnected {
		c.mutex.Unlock()
		return ErrNotConnected
	}

	c.disconnect()
	c.mutex.Unlock()

	c.sio.onDisconnect(c)
	return nil
}

// Handle takes over an http responseWriter/req -pair using the given Transport.
// If the HTTP method is POST then request's data-field will be used as an incoming
// message and the request is dropped. If the method is GET then a new socket encapsulating
// the request is created and a new connection is establised (or the connection will be
// reconnected). Finally, handle will wake up the reader and the flusher.
func (c *Conn) handle(t Transport, w http.ResponseWriter, req *http.Request) (err error) {
	c.mutex.Lock()

	if c.disconnected {
		c.mutex.Unlock()
		return ErrNotConnected
	}

	if req.Method == "POST" {
		c.mutex.Unlock()

		if msg := req.FormValue("data"); msg != "" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write(okResponse)
			c.receive([]byte(msg))
		} else {
			c.sio.Log("sio/conn: handle: POST missing data-field:", c)
			err = errMissingPostData
		}

		return
	}

	didHandshake := false

	s := t.newSocket()
	err = s.accept(w, req, func() {
		if c.socket != nil {
			c.socket.Close()
		}
		c.socket = s
		c.online = true
		c.lastConnected = time.Now()

		if !c.handshaked {
			// the connection has not been handshaked yet.
			if err = c.handshake(); err != nil {
				c.sio.Log("sio/conn: handle/handshake:", err, c)
				c.socket.Close()
				return
			}

			c.raddr = req.RemoteAddr
			c.handshaked = true
			didHandshake = true

			go c.keepalive()
			go c.flusher()
			go c.reader()

			c.sio.Log("sio/conn: connected:", c)
		} else {
			c.sio.Log("sio/conn: reconnected:", c)
		}

		c.numConns++

		select {
		case c.wakeupFlusher <- 1:
		default:
		}

		select {
		case c.wakeupReader <- 1:
		default:
		}

		if didHandshake {
			c.mutex.Unlock()
			c.sio.onConnect(c)
		}
	})

	if !didHandshake {
		c.mutex.Unlock()
	}

	return
}

// Handshake sends the handshake to the socket.
func (c *Conn) handshake() error {
	return c.enc.Encode(c.socket, handshake(c.sessionid))
}

func (c *Conn) disconnect() {
	c.sio.Log("sio/conn: disconnected:", c)
	c.socket.Close()
	c.disconnected = true
	close(c.wakeupFlusher)
	close(c.wakeupReader)
	close(c.queue)
	close(c.serviceQueue)
}

// Receive decodes and handles data received from the socket.
// It uses c.sio.codec to decode the data. The received non-heartbeat
// messages (frames) are then passed to c.sio.onMessage method and the
// heartbeats are processed right away (TODO).
func (c *Conn) receive(data []byte) {
	c.decBuf.Write(data)
	msgs, err := c.dec.Decode()

	if err != nil {
		c.sio.Log("sio/conn: receive/decode:", err, c)
		return
	}

	for _, m := range msgs {
		if hb, ok := m.heartbeat(); ok {
			c.lastHeartbeat = hb
		} else {
			c.sio.onMessage(c, m)
		}
	}
}

func (c *Conn) keepalive() {
	// If the reconnect interval was set to be lower than heartbeat, we need to
	// adjust the heartbeat to match so that we evaluate fast enough to catch the
	// reconnect timeout accurately
	var interval time.Duration
	if c.sio.config.ReconnectTimeout < c.sio.config.HeartbeatInterval {
		interval = c.sio.config.ReconnectTimeout
	} else {
		interval = c.sio.config.HeartbeatInterval
	}

	c.ticker = NewDelayTimer()
	c.ticker.Reset(interval)
	defer c.ticker.Stop()

Loop:
	for t := range c.ticker.Timeouts {
		c.mutex.Lock()
		if c.disconnected {
			c.mutex.Unlock()
			return
		}

		if (!c.online && t.Sub(c.lastDisconnected) > c.sio.config.ReconnectTimeout) ||
			(c.online && int(c.lastHeartbeat) < c.numHeartbeats) {

			c.disconnect()
			c.mutex.Unlock()
			break Loop
		}
		c.numHeartbeats++

		c.ticker.Reset(interval)

		select {
		case c.serviceQueue <- heartbeat(c.numHeartbeats):
		default:
			c.sio.Log("sio/keepalive: unable to queue heartbeat. fail now. TODO: FIXME", c)
			c.disconnect()
			c.mutex.Unlock()
			break Loop
		}

		c.mutex.Unlock()
	}
	c.sio.onDisconnect(c)
}

// Flusher waits for messages on the queue. It then
// tries to write the messages to the underlaying socket and
// will keep on trying until the wakeupFlusher is killed or the payload
// can be delivered. It is responsible for persisting messages until they
// can be succesfully delivered. No more than c.sio.config.QueueLength messages
// should ever be waiting for a delivery.
//
// NOTE: the c.sio.config.QueueLength is not a "hard limit", because one could have
// max amount of messages waiting in the queue and in the payload itself
// simultaneously.
func (c *Conn) flusher() {
	buf := new(bytes.Buffer)
	var err error
	var msg interface{}
	var n int
	var ok bool

	for {
		buf.Reset()
		msg = nil
		err = nil

		select {

		case msg, ok = <-c.serviceQueue:
			if !ok {
				return
			}
			err = c.enc.Encode(buf, msg)

		case msg, ok = <-c.queue:
			if !ok {
				return
			}

			err = c.enc.Encode(buf, msg)
			n = 1

			if err == nil {

			DrainLoop:
				for n < c.sio.config.QueueLength {
					select {
					case msg = <-c.queue:
						n++
						if err = c.enc.Encode(buf, msg); err != nil {
							break DrainLoop
						}

					default:
						break DrainLoop
					}
				}
			}
		}

		if err != nil {
			c.sio.Logf("sio/conn: flusher/encode: lost %d messages (%d bytes): %s %s", n, buf.Len(), err, c)
			continue
		}

	FlushLoop:
		for {
			for {
				c.mutex.Lock()
				_, err = buf.WriteTo(c.socket)
				c.mutex.Unlock()
				if err == nil {
					break FlushLoop
				} else if err != syscall.EAGAIN {
					break
				}
			}

			if _, ok := <-c.wakeupFlusher; !ok {
				return
			}
		}

	}
}

// Reader reads from the c.socket until the c.wakeupReader is closed.
// It is responsible for detecting unrecoverable read errors and timeouting
// the connection. When a read fails previously mentioned reasons, it will
// call the c.disconnect method and start waiting for the next event on the
// c.wakeupReader channel.
func (c *Conn) reader() {
	buf := make([]byte, c.sio.config.ReadBufferSize)

	for {
		c.mutex.Lock()
		socket := c.socket
		c.mutex.Unlock()

		for {
			nr, err := socket.Read(buf)
			if err != nil {
				if err != syscall.EAGAIN {
					if neterr, ok := err.(*net.OpError); ok && neterr.Timeout() {
						c.sio.Log("sio/conn: lost connection (timeout):", c)
						socket.Write(emptyResponse)
					} else {
						c.sio.Log("sio/conn: lost connection:", c)
					}
					break
				}
			} else if nr < 0 {
				break
			} else if nr > 0 {
				c.receive(buf[0:nr])
			}
		}

		c.mutex.Lock()
		c.lastDisconnected = time.Now()
		socket.Close()
		if c.socket == socket {
			c.online = false
		}
		c.mutex.Unlock()

		if _, ok := <-c.wakeupReader; !ok {
			break
		}
	}
}
