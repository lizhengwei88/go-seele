/**
*  @file
*  @copyright defined in go-seele/LICENSE
 */

package light

import (
	"errors"
	"fmt"
	rand2 "math/rand"
	"sync"
	"time"

	"github.com/seeleteam/go-seele/common"
	"github.com/seeleteam/go-seele/log"
	"github.com/seeleteam/go-seele/p2p"
)

var (
	errNoMorePeers   = errors.New("No peers found")
	errServiceQuited = errors.New("Service has quited")
)

type odrBackend struct {
	lock       sync.Mutex
	msgCh      chan *p2p.Message
	quitCh     chan struct{}
	requestMap map[uint32]chan interface{}
	wg         sync.WaitGroup
	peers      *peerSet
	log        *log.SeeleLog
}

func newOdrBackend(log *log.SeeleLog) *odrBackend {
	o := &odrBackend{
		msgCh:      make(chan *p2p.Message),
		requestMap: make(map[uint32]chan interface{}),
		quitCh:     make(chan struct{}),
		log:        log,
	}

	return o
}

func (o *odrBackend) start(peers *peerSet) {
	o.peers = peers
	o.wg.Add(1)
	go o.run()
}

func (o *odrBackend) run() {
	defer o.wg.Done()
loopOut:
	for {
		select {
		case msg := <-o.msgCh:
			o.handleResponse(msg)
		case <-o.quitCh:
			break loopOut
		}
	}
}

func (o *odrBackend) handleResponse(msg *p2p.Message) {
	factory, ok := odrResponseFactories[msg.Code]
	if !ok {
		return
	}

	response := factory()
	if err := common.Deserialize(msg.Payload, response); err != nil {
		o.log.Error("Failed to deserialize ODR response, code = %v, error = %v", msg.Code, err.Error())
		return
	}

	o.lock.Lock()
	defer o.lock.Unlock()

	if reqCh, ok := o.requestMap[response.getRequestID()]; ok {
		delete(o.requestMap, response.getRequestID())
		reqCh <- response
	}
}

func (o *odrBackend) getReqInfo() (uint32, chan interface{}, []*peer, error) {
	peerL := o.peers.choosePeers(common.LocalShardNumber)
	if len(peerL) == 0 {
		return 0, nil, nil, errNoMorePeers
	}
	rand2.Seed(time.Now().UnixNano())
	reqID := rand2.Uint32()
	ch := make(chan interface{})

	o.lock.Lock()
	if o.requestMap[reqID] != nil {
		panic("reqid conflicks")
	}

	o.requestMap[reqID] = ch
	o.lock.Unlock()
	return reqID, ch, peerL, nil
}

func (o *odrBackend) sendRequest(request odrRequest) error {
	reqID, ch, peerL, err := o.getReqInfo()
	if err != nil {
		return err
	}
	defer close(ch)

	request.setRequestID(reqID)
	code, payload := request.code(), common.SerializePanic(request)
	for _, p := range peerL {
		o.log.Debug("peer send request, code = %v, payloadSizeBytes = %v", code, len(payload))
		if err = p2p.SendMessage(p.rw, code, payload); err != nil {
			o.log.Info("Failed to send message with peer %v", p)
			return err
		}
	}

	timeout := time.NewTimer(msgWaitTimeout)
	defer timeout.Stop()

	select {
	case msg := <-ch:
		request.handleResponse(msg)
		return nil
	case <-o.quitCh:
		return errServiceQuited
	case <-timeout.C:
		o.lock.Lock()
		delete(o.requestMap, reqID)
		o.lock.Unlock()
		return fmt.Errorf("wait for msg reqid=%d timeout", reqID)
	}
}

func (o *odrBackend) close() {
	select {
	case <-o.quitCh:
	default:
		close(o.quitCh)
	}

	o.wg.Wait()
	close(o.msgCh)
}
