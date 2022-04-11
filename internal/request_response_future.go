package internal

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"

	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/apache/rocketmq-client-go/v2/rlog"
)

var RequestResponseFutureMap = NewRequestResponseFutureMap()

type requestResponseFutureCache struct {
	cache *cache.Cache
}

func NewRequestResponseFutureMap() *requestResponseFutureCache {
	tmpRrfCache := requestResponseFutureCache{
		cache: cache.New(5*time.Minute, 10*time.Minute),
	}

	// OnEvicted delete the timeout RequestResponseFuture, trigger set the failure cause.
	tmpRrfCache.cache.OnEvicted(func(s string, i interface{}) {
		rrf, ok := i.(*RequestResponseFuture)
		if !ok {
			rlog.Error("convert i to RequestResponseFuture err", map[string]interface{}{
				"correlationId": s,
			})
			return
		}
		if !rrf.IsTimeout() {
			return
		}

		err := fmt.Errorf("correlationId:%s request timeout, no reply message", s)
		rrf.CauseErr = err
		rrf.ExecuteRequestCallback()
	})
	return &tmpRrfCache
}

// SetRequestResponseFuture set rrf to map
func (fm *requestResponseFutureCache) SetRequestResponseFuture(rrf *RequestResponseFuture) {
	fm.cache.Set(rrf.CorrelationId, rrf, rrf.Timeout)
}

// SetResponseToRequestResponseFuture set reply to rrf
func (fm *requestResponseFutureCache) SetResponseToRequestResponseFuture(correlationId string, reply *primitive.Message) error {
	rrf, exist := fm.RequestResponseFuture(correlationId)
	if !exist {
		return errors.Wrapf(nil, "correlationId:%s not exist in map", correlationId)
	}
	rrf.PutResponseMessage(reply)
	if rrf.RequestCallback != nil {
		rrf.ExecuteRequestCallback()
	}
	return nil
}

// RequestResponseFuture get rrf from map by the CorrelationId
func (fm *requestResponseFutureCache) RequestResponseFuture(correlationId string) (*RequestResponseFuture, bool) {
	res, exists := fm.cache.Get(correlationId)
	if exists {
		return res.(*RequestResponseFuture), exists
	}
	return nil, exists
}

// RemoveRequestResponseFuture remove the rrf from map
func (fm *requestResponseFutureCache) RemoveRequestResponseFuture(correlationId string) {
	fm.cache.Delete(correlationId)
}

type RequestCallback func(ctx context.Context, msg *primitive.Message, err error)

// RequestResponseFuture store the rpc request. When producer wait for the response, get RequestResponseFuture.
type RequestResponseFuture struct {
	CorrelationId   string
	mtx             sync.RWMutex
	ResponseMsg     *primitive.Message
	Timeout         time.Duration
	RequestCallback RequestCallback
	SendRequestOk   bool
	Done            chan struct{}
	CauseErr        error
	BeginTime       time.Time
}

func NewRequestResponseFuture(correlationId string, timeout time.Duration, callback RequestCallback) *RequestResponseFuture {
	return &RequestResponseFuture{
		CorrelationId:   correlationId,
		Timeout:         timeout,
		RequestCallback: callback,
		Done:            make(chan struct{}),
		BeginTime:       time.Now(),
	}
}

func (rf *RequestResponseFuture) ExecuteRequestCallback() {
	if rf.RequestCallback == nil {
		return
	}

	rf.RequestCallback(context.Background(), rf.ResponseMsg, rf.CauseErr)
}

func (rf *RequestResponseFuture) WaitResponseMessage(reqMsg *primitive.Message) (*primitive.Message, error) {
	select {
	case <-time.After(rf.Timeout):
		err := fmt.Errorf("send request message to %s OK, but wait reply message timeout %d ms", reqMsg.Topic, rf.Timeout.Milliseconds())
		rlog.Error(err.Error(), nil)
		return nil, err
	case <-rf.Done:
		rf.mtx.RLock()
		rf.mtx.RUnlock()
		return rf.ResponseMsg, nil
	}
}

func (rf *RequestResponseFuture) PutResponseMessage(message *primitive.Message) {
	rf.mtx.Lock()
	defer rf.mtx.Unlock()
	rf.ResponseMsg = message
	close(rf.Done)
}

func (rf *RequestResponseFuture) IsTimeout() bool {
	diff := time.Since(rf.BeginTime)
	return diff > rf.Timeout
}
