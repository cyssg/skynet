package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/4ad/doozer"
	"github.com/bketelsen/skynet"
	"github.com/bketelsen/skynet/pools"
	"github.com/bketelsen/skynet/rpc/bsonrpc"
	"github.com/bketelsen/skynet/service"
	"launchpad.net/mgo/v2/bson"
	"math/rand"
	"net"
	"path"
	"reflect"
	"strings"
	"sync"
	"time"
)

type ServiceClient struct {
	Log         skynet.Logger `json:"-"`
	cconfig     *skynet.ClientConfig
	query       *Query
	instances   map[string]servicePool
	muxChan     chan interface{}
	timeoutChan chan timeoutLengths

	retryTimeout  time.Duration
	giveupTimeout time.Duration
}

func newServiceClient(query *Query, c *Client) (sc *ServiceClient) {
	sc = &ServiceClient{
		Log:         c.Config.Log,
		cconfig:     c.Config,
		query:       query,
		instances:   make(map[string]servicePool, 0),
		muxChan:     make(chan interface{}),
		timeoutChan: make(chan timeoutLengths),
	}
	go sc.mux()
	go sc.monitorInstances()
	return
}

type instanceFileCollector struct {
	files []string
}

func (ic *instanceFileCollector) VisitDir(path string, f *doozer.FileInfo) bool {
	return true
}
func (ic *instanceFileCollector) VisitFile(path string, f *doozer.FileInfo) {
	ic.files = append(ic.files, path)
}

func (c *ServiceClient) monitorInstances() {
	// TODO: Let's watch doozer and keep this list up to date so we don't need to search it every time we spawn a new connection
	doozer := c.query.DoozerConn

	rev := doozer.GetCurrentRevision()

	ddir := c.query.makePath()

	var ifc instanceFileCollector
	errch := make(chan error)
	doozer.Walk(rev, ddir, &ifc, errch)
	select {
	case err := <-errch:
		c.Log.Item(err)
	default:
	}

	for _, file := range ifc.files {
		buf, _, err := doozer.Get(file, rev)
		if err != nil {
			c.Log.Item(err)
			continue
		}
		var s service.Service
		err = json.Unmarshal(buf, &s)
		if err != nil {
			c.Log.Item(err)
			continue
		}

		c.muxChan <- service.ServiceDiscovered{
			Service: &s,
		}
	}

	watchPath := path.Join(c.query.makePath(), "**")

	for {
		ev, err := doozer.Wait(watchPath, rev+1)
		rev = ev.Rev
		if err != nil {
			continue
		}

		var s service.Service

		buf := bytes.NewBuffer(ev.Body)

		err = json.Unmarshal(buf.Bytes(), &s)
		if err != nil {
			continue
		}

		parts := strings.Split(ev.Path, "/")

		if c.query.pathMatches(parts, ev.Path) {
			//key := s.Config.ServiceAddr.String()

			if s.Registered == true {
				c.muxChan <- service.ServiceDiscovered{
					Service: &s,
				}
			} else {
				c.muxChan <- service.ServiceRemoved{
					Service: &s,
				}
			}
		}
	}
}

func getConnectionFactory(s *service.Service) (factory pools.Factory) {
	factory = func() (pools.Resource, error) {
		conn, err := net.Dial("tcp", s.Config.ServiceAddr.String())

		if err != nil {
			// TODO: handle failure here and attempt to connect to a different instance
			return nil, errors.New("Failed to connect to service: " + s.Config.ServiceAddr.String())
		}

		// get the service handshake
		var sh skynet.ServiceHandshake
		decoder := bsonrpc.NewDecoder(conn)
		err = decoder.Decode(&sh)
		if err != nil {
			conn.Close()
			return nil, err
		}

		ch := skynet.ClientHandshake{}
		encoder := bsonrpc.NewEncoder(conn)
		err = encoder.Encode(ch)
		if err != nil {
			conn.Close()
			return nil, err
		}

		if !sh.Registered {
			// this service has unregistered itself, look elsewhere
			conn.Close()
			return factory()
		}

		resource := ServiceResource{
			rpcClient: bsonrpc.NewClient(conn),
			service:   s,
		}

		return resource, nil
	}
	return
}

type servicePool struct {
	service *service.Service
	pool    *pools.ResourcePool
}

type lightInstanceRequest struct {
	exclusions map[string]bool
	response   chan servicePool
}

func (lir lightInstanceRequest) excludes(key string) bool {
	return lir.exclusions == nil || !lir.exclusions[key]
}

type timeoutLengths struct {
	retry, giveup time.Duration
}

func (c *ServiceClient) mux() {
	var spSubscribers []lightInstanceRequest

	for {
		select {
		case mi := <-c.muxChan:
			switch m := mi.(type) {
			case service.ServiceDiscovered:
				sp := servicePool{
					service: m.Service,
					pool:    pools.NewResourcePool(getConnectionFactory(m.Service), c.cconfig.ConnectionPoolSize, c.cconfig.ConnectionPoolSize),
				}
				_, known := c.instances[m.Service.Config.ServiceAddr.String()]
				c.instances[m.Service.Config.ServiceAddr.String()] = sp
				if !known {
					c.Log.Item(m)
				}
				// send this instance to anyone who was waiting
				for _, sps := range spSubscribers {
					sps.response <- sp
				}
				// no one is waiting anymore
				spSubscribers = spSubscribers[:0]
			case service.ServiceRemoved:
				delete(c.instances, m.Service.Config.ServiceAddr.String())
				c.Log.Item(m)
			case lightInstanceRequest:
				sp, ok := c.getLightInstanceMux(m)
				if ok {
					m.response <- sp
				} else {
					//if one wasn't immediately available, wait for the next incoming
					spSubscribers = append(spSubscribers, m)
				}
			}
		case c.timeoutChan <- timeoutLengths{
			retry:  c.retryTimeout,
			giveup: c.giveupTimeout,
		}:

		}
	}
}

func (c *ServiceClient) SetTimeout(retryTimeout, giveupTimeout time.Duration) {
	c.muxChan <- timeoutLengths{
		retry:  retryTimeout,
		giveup: giveupTimeout,
	}
}

func (c *ServiceClient) GetTimeout() (retry, giveup time.Duration) {
	tls := <-c.timeoutChan
	retry, giveup = tls.retry, tls.giveup
	return
}

// do not call this from outside .mux()
func (c *ServiceClient) getLightInstanceMux(lir lightInstanceRequest) (sp servicePool, ok bool) {
	if len(c.instances) == 0 {
		ok = false
		return
	}

	// filter based on the provided exclusion map
	inclInstances := make([]servicePool, len(c.instances), 0)
	for _, i := range c.instances {
		if lir.excludes(getInstanceKey(i.service)) {
			continue
		}
		inclInstances = append(inclInstances, i)
	}

	if len(inclInstances) == 0 {
		ok = false
		return
	}

	// then choose one randomly

	ri := rand.Intn(len(inclInstances))
	sp = inclInstances[ri]
	ok = true

	return
}

func (c *ServiceClient) getLightInstance(exclusions map[string]bool) (sp servicePool) {
	response := make(chan servicePool, 1)
	c.muxChan <- lightInstanceRequest{
		exclusions: exclusions,
		response:   response,
	}
	sp = <-response
	return
}

// ServiceClient.trySend() tries to make an RPC request on a particular connection to an instance
func (c *ServiceClient) trySend(sr ServiceResource, requestInfo *skynet.RequestInfo, funcName string, in interface{}, outPointer interface{}) (err error) {
	if requestInfo == nil {
		requestInfo = &skynet.RequestInfo{
			RequestID: skynet.UUID(),
		}
	}

	sin := service.ServiceRPCIn{
		RequestInfo: requestInfo,
		Method:      funcName,
	}

	sin.In, err = bson.Marshal(in)
	if err != nil {
		return
	}

	sout := service.ServiceRPCOut{}

	// TODO: Check for connectivity issue so that we can try to get another resource out of the pool
	err = sr.rpcClient.Call(sr.service.Config.Name+".Forward", sin, &sout)
	if err != nil {
		sr.Close()
		c.Log.Item(err)
	}

	err = bson.Unmarshal(sout.Out, outPointer)
	if err != nil {
		return
	}

	return
}

func cloneOutDest(outDest interface{}) (clone interface{}) {
	outType := reflect.TypeOf(outDest)
	switch outType.Kind() {
	case reflect.Ptr:
		clonePtr := reflect.New(outType.Elem())
		clone = clonePtr.Interface()
	case reflect.Map:
		cloneMap := reflect.MakeMap(outType)
		clone = cloneMap.Interface()
	default:
		panic("illegal out type")
	}
	return
}

func copyOutDest(outDest interface{}, src interface{}) {
	outType := reflect.TypeOf(outDest)
	outVal := reflect.ValueOf(outDest)
	srcVal := reflect.ValueOf(src)
	switch outType.Kind() {
	case reflect.Ptr:
		outVal.Elem().Set(srcVal.Elem())
	case reflect.Map:
		for _, key := range srcVal.MapKeys() {
			val := srcVal.MapIndex(key)
			outVal.SetMapIndex(key, val)
		}
	default:
		panic("illegal out type")
	}

}

func (c *ServiceClient) Send(ri *skynet.RequestInfo, fn string, in interface{}, out interface{}) (err error) {
	retry, giveup := c.GetTimeout()

	attempts := make(chan sendAttempt)

	// the exclSend closure will try to send the request to one of the services without outstanding attempts
	var exclMutex sync.Mutex
	exclusions := make(map[string]bool)
	exclSend := func() {

		exclMutex.Lock()
		exclusionClone := make(map[string]bool)
		for key, v := range exclusions {
			if v {
				exclusionClone[key] = true
			}
		}
		exclMutex.Unlock()

		sp := c.getLightInstance(exclusionClone)

		exclMutex.Lock()
		exclusions[getInstanceKey(sp.service)] = true
		exclMutex.Unlock()

		defer func() {
			exclMutex.Lock()
			exclusions[getInstanceKey(sp.service)] = false
			exclMutex.Unlock()
		}()

		r, err := sp.pool.Acquire()
		defer sp.pool.Release(r)
		if err != nil {
			c.Log.Item(err)
			return
		}

		sr := r.(ServiceResource)

		outClone := cloneOutDest(out)

		at := sendAttempt{
			outClone: outClone,
			err:      c.trySend(sr, ri, fn, in, outClone),
		}

		attempts <- at

	}

	go exclSend()

	var ticks <-chan time.Time
	if retry > 0 {
		ticks = time.NewTicker(retry).C
	}
	var timeout <-chan time.Time
	if giveup > 0 {
		timeout = time.NewTimer(giveup).C
	}

	for {
		select {
		case <-ticks:
			go exclSend()
		case <-timeout:
			if err == nil {
				err = ErrRequestTimeout
			}
			// otherwise use the last error reported from an attempt
			return
		case attempt := <-attempts:
			err = attempt.err
			if err == nil {
				copyOutDest(out, attempt.outClone)
				return
			}
		}
	}

	return
}

type sendAttempt struct {
	outClone interface{}
	err      error
}

func (c *ServiceClient) sendRetry(giveup time.Duration, ri *skynet.RequestInfo, fn string, in interface{}, out interface{}, attempts chan sendAttempt) {
	outClone := cloneOutDest(out)

	at := sendAttempt{
		outClone: outClone,
		err:      c.SendOnce(giveup, ri, fn, in, outClone),
	}

	attempts <- at

	return
}

func (c *ServiceClient) SendOnce(giveup time.Duration, requestInfo *skynet.RequestInfo, funcName string, in interface{}, outPointer interface{}) (err error) {
	// TODO: timeout logic

	sp := c.getLightInstance(nil)

	r, err := sp.pool.Acquire()
	if err != nil {
		c.Log.Item(err)
		return
	}

	sr := r.(ServiceResource)
	err = c.trySend(sr, requestInfo, funcName, in, outPointer)
	if err != nil {
		c.Log.Item(err)
		return
	}

	sp.pool.Release(sr)

	return
}

func (c *ServiceClient) isClosed(service ServiceResource) bool {
	key := getInstanceKey(service.service)

	if _, ok := c.instances[key]; ok {
		return false
	}

	return true
}
