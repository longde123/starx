package network

import (
	"encoding/json"
	"errors"
	"net"
	"reflect"

	"github.com/chrislonng/starx/cluster"
	"github.com/chrislonng/starx/cluster/rpc"
	"github.com/chrislonng/starx/log"
	"github.com/chrislonng/starx/network/message"
	"github.com/chrislonng/starx/network/packet"
	"github.com/chrislonng/starx/network/route"
	"github.com/chrislonng/starx/session"
)

// Unhandled message buffer size
// Every connection has an individual message channel buffer
const (
	packetBufferSize = 256
)

var Handler = newHandlerService()

type unhandledPacket struct {
	agent  *agent
	packet *packet.Packet
}

type handlerService struct {
	serviceMap map[string]*service
}

func newHandlerService() *handlerService {
	return &handlerService{
		serviceMap: make(map[string]*service),
	}
}

// Register publishes in the service the set of methods of the
// receiver value that satisfy the following conditions:
//	- exported method of exported type
//	- two arguments, both of exported type
//	- the first argument is *session.Session
//	- the second argument is []byte or a pointer
func (hs *handlerService) Register(rcvr Component) error {
	if hs.serviceMap == nil {
		hs.serviceMap = make(map[string]*service)
	}
	s := new(service)
	s.typ = reflect.TypeOf(rcvr)
	s.rcvr = reflect.ValueOf(rcvr)
	sname := reflect.Indirect(s.rcvr).Type().Name()
	if sname == "" {
		return errors.New("handler.Register: no service name for type " + s.typ.String())
	}
	if !isExported(sname) {
		return errors.New("handler.Register: type " + sname + " is not exported")

	}
	if _, present := hs.serviceMap[sname]; present {
		return errors.New("handler: service already defined: " + sname)
	}
	s.name = sname

	// Install the methods
	s.handlerMethod = suitableHandlerMethods(s.typ, true)

	if len(s.handlerMethod) == 0 {
		str := ""

		// To help the user, see if a pointer receiver would work.
		method := suitableHandlerMethods(reflect.PtrTo(s.typ), false)
		if len(method) != 0 {
			str = "handler.Register: type " + sname + " has no exported methods of suitable type (hint: pass a pointer to value of that type)"
		} else {
			str = "handler.Register: type " + sname + " has no exported methods of suitable type"
		}
		return errors.New(str)
	}
	hs.serviceMap[s.name] = s
	return nil
}

// Handle network connection
// Read data from Socket file descriptor and decode it, handle message in
// individual logic routine
func (hs *handlerService) Handle(conn net.Conn) {
	defer conn.Close()

	// message buffer
	packetChan := make(chan *unhandledPacket, packetBufferSize)
	endChan := make(chan bool, 1)

	// all user logic will be handled in single goroutine
	// synchronized in below routine
	go func() {
	loop:
		for {
			select {
			case p := <-packetChan:
				if p != nil {
					hs.processPacket(p.agent, p.packet)
				}
			case <-endChan:
				break loop
			}
		}

	}()

	// register new session when new connection connected in
	agent := defaultNetService.createAgent(conn)
	log.Debugf("new agent(%s)", agent.String())
	tmp := make([]byte, 0) // save truncated data
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Debugf("session closed, id: %d, ip: %s", agent.session.Id, agent.socket.RemoteAddr())
			close(packetChan)
			endChan <- true
			agent.close()
			break
		}
		tmp = append(tmp, buf[:n]...)
		var p *packet.Packet // save decoded packet
		for len(tmp) >= packet.HeadLength {
			p, tmp, err = packet.Unpack(tmp)
			if err != nil {
				agent.close()
				break
			}
			packetChan <- &unhandledPacket{agent: agent, packet: p}
		}
	}
}

func (hs *handlerService) processPacket(a *agent, p *packet.Packet) {
	switch p.Type {
	case packet.Handshake:
		a.status = statusHandshake
		data, err := json.Marshal(map[string]interface{}{
			"code": 200,
			"sys":  map[string]float64{"heartbeat": heartbeatInternal.Seconds()},
		})
		if err != nil {
			log.Infof(err.Error())
		}

		rp := &packet.Packet{
			Type:   packet.Handshake,
			Length: len(data),
			Data:   data,
		}

		resp, err := rp.Pack()
		if err != nil {
			log.Errorf(err.Error())
			a.close()
		}

		if err := a.Send(resp); err != nil {
			log.Errorf(err.Error())
			a.close()
		}
	case packet.HandshakeAck:
		a.status = statusWorking
	case packet.Data:
		m, err := message.Decode(p.Data)
		if err != nil {
			log.Errorf(err.Error())
			return
		}
		hs.processMessage(a.session, m)
		fallthrough
	case packet.Heartbeat:
		go a.heartbeat()
	default:
		log.Infof("invalid packet type")
		a.close()
	}
}

func (hs *handlerService) processMessage(session *session.Session, msg *message.Message) {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("processMessage Error: %+v", err)
		}
	}()

	switch msg.Type {
	case message.Request:
		session.LastID = msg.ID
	case message.Notify:
		session.LastID = 0
	default:
		log.Errorf("invalid message type")
		return
	}

	log.Debugf(msg.String())
	r, err := route.Decode(msg.Route)
	if err != nil {
		log.Errorf(err.Error())
		return
	}

	// current server as default server type
	if r.ServerType == "" {
		r.ServerType = appConfig.Type
	}

	// message dispatch
	if r.ServerType == appConfig.Type {
		hs.localProcess(session, r, msg)
	} else {
		hs.remoteProcess(session, r, msg)
	}
}

// current message handle in local server
func (hs *handlerService) localProcess(session *session.Session, route *route.Route, msg *message.Message) {
	s, ok := hs.serviceMap[route.Service]
	if !ok || s == nil {
		log.Infof("handler: service: " + route.Service + " not found")
		return
	}

	m, ok := s.handlerMethod[route.Method]
	if !ok || m == nil {
		log.Infof("handler: " + route.Service + " does not contain method: " + route.Method)
		return
	}

	var data interface{}
	if m.raw {
		data = msg.Data
	} else {
		data = reflect.New(m.dataType.Elem()).Interface()
		err := serializer.Deserialize(msg.Data, data)
		if err != nil {
			log.Errorf("deserialize error: %s", err.Error())
			return
		}
	}

	ret := m.method.Func.Call([]reflect.Value{s.rcvr, reflect.ValueOf(session), reflect.ValueOf(data)})
	if len(ret) > 0 {
		err := ret[0].Interface()
		if err != nil {
			log.Errorf(err.(error).Error())
		}
	}
}

// current message handle in remote server
func (hs *handlerService) remoteProcess(session *session.Session, route *route.Route, msg *message.Message) {
	if _, err := cluster.Call(rpc.Sys, route, session, msg.Data); err != nil {
		log.Errorf(err.Error())
	}
}

func (hs *handlerService) dumpServiceMap() {
	for sname, s := range hs.serviceMap {
		for mname, _ := range s.handlerMethod {
			log.Infof("registered service: %s.%s", sname, mname)
		}
	}
}
