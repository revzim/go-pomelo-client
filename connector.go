package client

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	"github.com/revzim/go-pomelo-client/codec"
	"github.com/revzim/go-pomelo-client/message"
	"github.com/revzim/go-pomelo-client/packet"
)

type (
	// Connector is a Pomelo [nano] client
	Connector struct {
		conn              net.Conn       // low-level connection
		codec             *codec.Decoder // decoder
		mid               uint           // message id
		muConn            sync.RWMutex
		connecting        bool        // connection status
		die               chan byte   // connector close channel
		chSend            chan []byte // send queue
		connectedCallback func()

		// some packet data
		handshakeData    []byte // handshake data
		handshakeAckData []byte // handshake ack data
		heartbeatData    []byte // heartbeat data

		// events handler
		sync.RWMutex
		events map[string]Callback

		// response handler
		muResponses sync.RWMutex
		responses   map[uint]Callback
	}
	// DefaultACK --
	DefaultHandshakePacket struct {
		Code int              `json:"code"`
		Sys  HeartbeatSysOpts `json:"sys"`
	}
	// HeartbeatSysOpts --
	HeartbeatSysOpts struct {
		Heartbeat int `json:"heartbeat"`
	}

	// SysOpts --
	SysOpts struct {
		Version string                 `json:"version"`
		Type    string                 `json:"type"`
		RSA     map[string]interface{} `json:"rsa"`
	}

	// HandshakeOpts --
	HandshakeOpts struct {
		Sys      SysOpts                `json:"sys"`
		UserData map[string]interface{} `json:"user"`
	}
)

// SetHandshake --
func (c *Connector) SetHandshake(handshake interface{}) error {
	data, err := json.Marshal(handshake)
	if err != nil {
		return err
	}

	c.handshakeData, err = codec.Encode(packet.Handshake, data)
	if err != nil {
		return err
	}

	return nil
}

// SetHandshakeAck --
func (c *Connector) SetHandshakeAck(handshakeAck interface{}) error {
	var err error
	if handshakeAck == nil {
		c.handshakeAckData, err = codec.Encode(packet.HandshakeAck, nil)
		if err != nil {
			return err
		}
		return nil
	}

	data, err := json.Marshal(handshakeAck)
	if err != nil {
		return err
	}

	c.handshakeAckData, err = codec.Encode(packet.HandshakeAck, data)
	if err != nil {
		return err
	}

	return nil
}

// SetHeartBeat --
func (c *Connector) SetHeartBeat(heartbeat interface{}) error {
	var err error
	if heartbeat == nil {
		c.heartbeatData, err = codec.Encode(packet.Heartbeat, nil)
		if err != nil {
			return err
		}
		return nil
	}
	data, err := json.Marshal(heartbeat)
	if err != nil {
		return err
	}

	c.heartbeatData, err = codec.Encode(packet.Heartbeat, data)
	if err != nil {
		return err
	}

	return nil
}

// Connected --
func (c *Connector) Connected(cb func()) {
	c.connectedCallback = cb
}

// InitReqHandshake --
// func (c *Connector) InitReqHandshake(opts *HandshakeOpts) error {
// 	return c.SetHandshake(opts)
// }

// InitReqHandshake --
func (c *Connector) InitReqHandshake(version, hType string, rsa, userData map[string]interface{}) error {
	return c.SetHandshake(&HandshakeOpts{
		Sys: SysOpts{
			Version: version,
			Type:    hType,
			RSA:     rsa,
		},
		UserData: userData,
	})
}

// InitHandshakeACK --
func (c *Connector) InitHandshakeACK(heartbeatDuration int) error {
	ackDataMap := &DefaultHandshakePacket{
		Code: 200,
		Sys: HeartbeatSysOpts{
			Heartbeat: 1,
		},
	}
	return c.SetHandshakeAck(ackDataMap)
}

// Run --
func (c *Connector) Run(addr string, ws bool) error {
	if c.handshakeData == nil {
		return errors.New("handshake not defined")
	}

	if c.handshakeAckData == nil {
		err := c.SetHandshakeAck(nil)
		if err != nil {
			return err
		}
	}

	if c.heartbeatData == nil {
		err := c.SetHeartBeat(nil)
		if err != nil {
			return err
		}
	}
	var err error
	var conn net.Conn
	if ws {
		conn, err = websocket.Dial(addr, addr, addr)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}

	c.conn = conn
	c.connecting = true

	go c.write()

	c.send(c.handshakeData)

	err = c.read()

	return err
}

// Request send a request to server and register a callbck for the response
func (c *Connector) Request(route string, data []byte, callback Callback) error {
	msg := &message.Message{
		Type:  message.Request,
		Route: route,
		ID:    c.mid,
		Data:  data,
	}

	c.setResponseHandler(c.mid, callback)
	if err := c.sendMessage(msg); err != nil {
		log.Println(err)
		c.setResponseHandler(c.mid, nil)
		return err
	}

	return nil
}

// Notify send a notification to server
func (c *Connector) Notify(route string, data []byte) error {
	msg := &message.Message{
		Type:  message.Notify,
		Route: route,
		Data:  data,
	}
	return c.sendMessage(msg)
}

// On add the callback for the event
func (c *Connector) On(event string, callback Callback) {
	c.Lock()
	defer c.Unlock()

	c.events[event] = callback
}

// Close close the connection, and shutdown the benchmark
func (c *Connector) Close() {
	if !c.connecting {
		return
	}
	c.conn.Close()
	c.die <- 1
	c.connecting = false
}

// IsClosed check the connection is closed
func (c *Connector) IsClosed() bool {
	return !c.connecting
}

func (c *Connector) eventHandler(event string) (Callback, bool) {
	c.RLock()
	defer c.RUnlock()

	cb, ok := c.events[event]
	return cb, ok
}

func (c *Connector) responseHandler(mid uint) (Callback, bool) {
	c.muResponses.RLock()
	defer c.muResponses.RUnlock()

	cb, ok := c.responses[mid]
	return cb, ok
}

func (c *Connector) setResponseHandler(mid uint, cb Callback) {
	c.muResponses.Lock()
	defer c.muResponses.Unlock()

	if cb == nil {
		delete(c.responses, mid)
	} else {
		c.responses[mid] = cb
	}
}

func (c *Connector) sendMessage(msg *message.Message) error {
	data, err := msg.Encode()
	if err != nil {
		return err
	}
	// log.Printf("%+v | %+v | %+v\n", msg.Data, msg, data)

	payload, err := codec.Encode(packet.Data, data)
	if err != nil {
		return err
	}

	c.mid++
	c.send(payload)

	return nil
}

func (c *Connector) write() {
	for {
		select {
		case data := <-c.chSend:
			if c.conn != nil {
				if _, err := c.conn.Write(data); err != nil {
					log.Println("conn write err", err.Error())
					// c.Close()
				}
			}

		case <-c.die:
			return
		}
	}
}

func (c *Connector) send(data []byte) {
	c.chSend <- data
}

func (c *Connector) read() error {
	buf := make([]byte, 2048)

	for {
		time.Sleep(time.Second)
		if c.IsClosed() {
			return errors.New("read err: connector is closed")
		}
		n, err := c.conn.Read(buf)
		if err != nil {
			log.Println("connector read err", err.Error())
			c.Close()
			return err
			// continue
		}

		packets, err := c.codec.Decode(buf[:n])
		if err != nil {
			log.Println("connector read decode err", err.Error())
			// c.Close()
			// return
			continue
		}

		for i := range packets {
			p := packets[i]
			// log.Println("packet-->", p)
			c.processPacket(p)
		}
	}
}

func (c *Connector) processPacket(p *packet.Packet) {
	// log.Printf("packet: %+v\n", p)
	switch p.Type {
	case packet.Handshake:
		var handshakeResp DefaultHandshakePacket
		err := json.Unmarshal(p.Data, &handshakeResp)
		if err != nil {
			c.Close()
			return
		}
		log.Println(handshakeResp.Code)
		if handshakeResp.Code == 200 {
			go func() {
				ticker := time.NewTicker(time.Second * time.Duration(handshakeResp.Sys.Heartbeat))
				for range ticker.C {
					if c.IsClosed() {
						return
					}
					c.send(c.heartbeatData)
				}
			}()
			c.send(c.handshakeAckData)
			if c.connectedCallback != nil {
				c.connectedCallback()
			}
		} else {
			log.Fatal("bad packet handshake code, not 200:", string(p.Data))
			c.Close()
		}
	case packet.Data:
		msg, err := message.Decode(p.Data)
		if err != nil {
			return
		}
		c.processMessage(msg)

	case packet.Kick:
		log.Fatal("server kick -->", p)
		c.Close()
	}
}

func (c *Connector) processMessage(msg *message.Message) {
	switch msg.Type {
	case message.Push:
		cb, ok := c.eventHandler(msg.Route)
		if !ok {
			log.Println("event handler not found", msg.Route)
			return
		}

		cb(msg.Data)

	case message.Response:
		cb, ok := c.responseHandler(msg.ID)
		if !ok {
			log.Println("response handler not found", msg.ID)
			return
		}

		cb(msg.Data)
		c.setResponseHandler(msg.ID, nil)
	}
}
