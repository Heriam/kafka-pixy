package msgstream

import (
	"fmt"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/mailgun/kafka-pixy/actor"
	"github.com/mailgun/kafka-pixy/consumer"
	"github.com/mailgun/kafka-pixy/consumer/mapper"
	"github.com/mailgun/kafka-pixy/none"
	"github.com/mailgun/log"
)

// Factory provides API to spawn message streams to that read message from
// topic partitions. It ensures that there is only on message stream for a
// particular topic partition at a time.
type Factory interface {
	// SpawnMessageStream creates a T instance for the given topic/partition
	// with the given offset. It will return an error if there is an instance
	// already consuming from the topic/partition.
	//
	// Offset can be a literal offset, or OffsetNewest or OffsetOldest. If
	// offset is smaller then the oldest offset then the oldest offset is
	// returned. If offset is larger then the newest offset then the newest
	// offset is returned. If offset is either sarama.OffsetNewest or
	// sarama.OffsetOldest constant, then the actual offset value is returned.
	// otherwise offset is returned.
	SpawnMessageStream(namespace *actor.ID, topic string, partition int32, offset int64) (T, int64, error)

	// Stop shuts down the consumer. It must be called after all child partition
	// consumers have already been closed.
	Stop()
}

// T fetched messages from a given topic and partition.
type T interface {
	// Messages returns the read channel for the messages that are fetched from
	// the topic partition.
	Messages() <-chan *consumer.Message

	// Errors returns a read channel of errors that occurred during consuming,
	// if enabled. By default, errors are logged and not returned over this
	// channel. If you want to implement any custom error handling, set your
	// config's Consumer.Return.Errors setting to true, and read from this
	// channel.
	Errors() <-chan *Err

	// Stop synchronously stops the partition consumer. It must be called
	// before the factory that created the instance can be stopped.
	Stop()
}

// Err is what is provided to the user when an error occurs.
// It wraps an error and includes the topic and partition.
type Err struct {
	Topic     string
	Partition int32
	Err       error
}

func (ce Err) Error() string {
	return fmt.Sprintf("kafka: error while consuming %s/%d: %s", ce.Topic, ce.Partition, ce.Err)
}

// ConsumerErrors is a type that wraps a batch of errors and implements the Error interface.
// It can be returned from the PartitionConsumer's Close methods to avoid the need to manually drain errors
// when stopping.
type Errors []*Err

func (ce Errors) Error() string {
	return fmt.Sprintf("kafka: %d errors while consuming", len(ce))
}

type factory struct {
	namespace    *actor.ID
	saramaCfg    *sarama.Config
	client       sarama.Client
	children     map[instanceID]*msgStream
	childrenLock sync.Mutex
	mapper       *mapper.T
}

type instanceID struct {
	topic     string
	partition int32
}

// SpawnFactory creates a new message stream factory using the given client. It
// is still necessary to call Stop() on the underlying client after shutting
// down this factory.
func SpawnFactory(namespace *actor.ID, client sarama.Client) (Factory, error) {
	f := &factory{
		namespace: namespace.NewChild("msg_stream_f"),
		client:    client,
		saramaCfg: client.Config(),
		children:  make(map[instanceID]*msgStream),
	}
	f.mapper = mapper.Spawn(f.namespace, f)
	return f, nil
}

// implements `Factory`.
func (f *factory) SpawnMessageStream(namespace *actor.ID, topic string, partition int32, offset int64) (T, int64, error) {
	concreteOffset, err := f.chooseStartingOffset(topic, partition, offset)
	if err != nil {
		return nil, sarama.OffsetNewest, err
	}

	f.childrenLock.Lock()
	defer f.childrenLock.Unlock()

	id := instanceID{topic, partition}
	if _, ok := f.children[id]; ok {
		return nil, sarama.OffsetNewest, sarama.ConfigurationError("That topic/partition is already being consumed")
	}
	ms := f.spawnMsgStream(namespace, id, concreteOffset)
	f.mapper.WorkerSpawned() <- ms
	f.children[id] = ms
	return ms, concreteOffset, nil
}

// implements `Factory`.
func (f *factory) Stop() {
	f.mapper.Stop()
}

// implements `mapper.Resolver.ResolveBroker()`.
func (f *factory) ResolveBroker(pw mapper.Worker) (*sarama.Broker, error) {
	ms := pw.(*msgStream)
	if err := f.client.RefreshMetadata(ms.id.topic); err != nil {
		return nil, err
	}
	return f.client.Leader(ms.id.topic, ms.id.partition)
}

// implements `mapper.Resolver.Executor()`
func (f *factory) SpawnExecutor(brokerConn *sarama.Broker) mapper.Executor {
	be := &brokerExecutor{
		aggrActorID:     f.namespace.NewChild("broker", brokerConn.ID(), "aggr"),
		execActorID:     f.namespace.NewChild("broker", brokerConn.ID(), "exec"),
		config:          f.saramaCfg,
		conn:            brokerConn,
		requestsCh:      make(chan fetchReq),
		batchRequestsCh: make(chan []fetchReq),
	}
	actor.Spawn(be.aggrActorID, &be.wg, be.runAggregator)
	actor.Spawn(be.execActorID, &be.wg, be.runExecutor)
	return be
}

// chooseStartingOffset takes an offset value that may be either an actual
// offset of two constants (`OffsetNewest` and `OffsetOldest`) and return an
// offset value. It checks if the offset value belongs to the current range.
//
// FIXME: The offset values corresponding to `OffsetNewest` and `OffsetOldest`
// may change during the function execution (e.g. an old log chunk gets
// deleted), so the offset value returned by the function may be incorrect.
func (f *factory) chooseStartingOffset(topic string, partition int32, offset int64) (int64, error) {
	newestOffset, err := f.client.GetOffset(topic, partition, sarama.OffsetNewest)
	if err != nil {
		return 0, err
	}
	oldestOffset, err := f.client.GetOffset(topic, partition, sarama.OffsetOldest)
	if err != nil {
		return 0, err
	}

	switch {
	case offset == sarama.OffsetNewest || offset > newestOffset:
		return newestOffset, nil
	case offset == sarama.OffsetOldest || offset < oldestOffset:
		return oldestOffset, nil
	default:
		return offset, nil
	}
}

// implements `mapper.Worker`.
type msgStream struct {
	actorID      *actor.ID
	f            *factory
	id           instanceID
	fetchSize    int32
	offset       int64
	lag          int64
	assignmentCh chan mapper.Executor
	initErrorCh  chan error
	messagesCh   chan *consumer.Message
	errorsCh     chan *Err
	closingCh    chan none.T
	wg           sync.WaitGroup

	assignedBrokerRequestCh   chan<- fetchReq
	nilOrBrokerRequestsCh     chan<- fetchReq
	nilOrReassignRetryTimerCh <-chan time.Time
	lastReassignTime          time.Time
}

func (f *factory) spawnMsgStream(namespace *actor.ID, id instanceID, offset int64) *msgStream {
	ms := &msgStream{
		actorID:      namespace.NewChild("msg_stream"),
		f:            f,
		id:           id,
		assignmentCh: make(chan mapper.Executor, 1),
		initErrorCh:  make(chan error),
		messagesCh:   make(chan *consumer.Message, f.saramaCfg.ChannelBufferSize),
		errorsCh:     make(chan *Err, f.saramaCfg.ChannelBufferSize),
		closingCh:    make(chan none.T, 1),
		offset:       offset,
		fetchSize:    f.saramaCfg.Consumer.Fetch.Default,
	}
	actor.Spawn(ms.actorID, &ms.wg, ms.run)
	return ms
}

// implements `Factory`.
func (ms *msgStream) Messages() <-chan *consumer.Message {
	return ms.messagesCh
}

// implements `Factory`.
func (ms *msgStream) Errors() <-chan *Err {
	return ms.errorsCh
}

// implements `Factory`.
func (ms *msgStream) Stop() {
	close(ms.closingCh)
	ms.wg.Wait()

	ms.f.childrenLock.Lock()
	delete(ms.f.children, ms.id)
	ms.f.childrenLock.Unlock()
	ms.f.mapper.WorkerStopped() <- ms
}

// implements `mapper.Worker`.
func (ms *msgStream) Assignment() chan<- mapper.Executor {
	return ms.assignmentCh
}

// pullMessages sends fetched requests to the broker executor assigned by the
// redispatch goroutine; parses broker fetch responses and pushes parsed
// `ConsumerMessages` to the message channel. It tries to keep the message
// channel buffer full making fetch requests to the assigned broker as needed.
func (ms *msgStream) run() {
	var (
		fetchResultCh       = make(chan fetchRes, 1)
		nilOrFetchResultsCh <-chan fetchRes
		nilOrMessagesCh     chan<- *consumer.Message
		fetchedMessages     []*consumer.Message
		err                 error
		currMessage         *consumer.Message
		currMessageIdx      int
	)
pullMessagesLoop:
	for {
		select {
		case bw := <-ms.assignmentCh:
			log.Infof("<%s> assigned %s", ms.actorID, bw)
			if bw == nil {
				ms.triggerOrScheduleReassign("no broker assigned")
				continue pullMessagesLoop
			}
			be := bw.(*brokerExecutor)
			// A new leader broker has been assigned for the partition.
			ms.assignedBrokerRequestCh = be.requestsCh
			// Cancel the reassign retry timer.
			ms.nilOrReassignRetryTimerCh = nil
			// If there is a fetch request pending, then let it complete,
			// otherwise trigger one.
			if nilOrFetchResultsCh == nil && nilOrMessagesCh == nil {
				ms.nilOrBrokerRequestsCh = ms.assignedBrokerRequestCh
			}

		case ms.nilOrBrokerRequestsCh <- fetchReq{ms.id.topic, ms.id.partition, ms.offset, ms.fetchSize, ms.lag, fetchResultCh}:
			ms.nilOrBrokerRequestsCh = nil
			nilOrFetchResultsCh = fetchResultCh

		case result := <-nilOrFetchResultsCh:
			nilOrFetchResultsCh = nil
			if fetchedMessages, err = ms.parseFetchResult(ms.actorID, result); err != nil {
				log.Infof("<%s> fetch failed: err=%s", ms.actorID, err)
				ms.reportError(err)
				if err == sarama.ErrOffsetOutOfRange {
					// There's no point in retrying this it will just fail the
					// same way, therefore is nothing to do but give up.
					goto done
				}
				ms.triggerOrScheduleReassign("fetch error")
				continue pullMessagesLoop
			}
			// If no messages has been fetched, then trigger another request.
			if len(fetchedMessages) == 0 {
				ms.nilOrBrokerRequestsCh = ms.assignedBrokerRequestCh
				continue pullMessagesLoop
			}
			// Some messages have been fetched, start pushing them to the user.
			currMessageIdx = 0
			currMessage = fetchedMessages[currMessageIdx]
			nilOrMessagesCh = ms.messagesCh

		case nilOrMessagesCh <- currMessage:
			ms.offset = currMessage.Offset + 1
			currMessageIdx++
			if currMessageIdx < len(fetchedMessages) {
				currMessage = fetchedMessages[currMessageIdx]
				continue pullMessagesLoop
			}
			// All messages have been pushed, trigger a new fetch request.
			nilOrMessagesCh = nil
			ms.nilOrBrokerRequestsCh = ms.assignedBrokerRequestCh

		case <-ms.nilOrReassignRetryTimerCh:
			ms.f.mapper.WorkerReassign() <- ms
			log.Infof("<%s> reassign triggered by timeout", ms.actorID)
			ms.nilOrReassignRetryTimerCh = time.After(ms.f.saramaCfg.Consumer.Retry.Backoff)

		case <-ms.closingCh:
			goto done
		}
	}
done:
	close(ms.messagesCh)
	close(ms.errorsCh)
}

func (ms *msgStream) triggerOrScheduleReassign(reason string) {
	ms.assignedBrokerRequestCh = nil
	now := time.Now().UTC()
	if now.Sub(ms.lastReassignTime) > ms.f.saramaCfg.Consumer.Retry.Backoff {
		log.Infof("<%s> trigger reassign: reason=(%s)", ms.actorID, reason)
		ms.lastReassignTime = now
		ms.f.mapper.WorkerReassign() <- ms
	} else {
		log.Infof("<%s> schedule reassign: reason=(%s)", ms.actorID, reason)
	}
	ms.nilOrReassignRetryTimerCh = time.After(ms.f.saramaCfg.Consumer.Retry.Backoff)
}

// parseFetchResult parses a fetch response received a broker.
func (ms *msgStream) parseFetchResult(cid *actor.ID, fetchResult fetchRes) ([]*consumer.Message, error) {
	if fetchResult.Err != nil {
		return nil, fetchResult.Err
	}

	response := fetchResult.Response
	if response == nil {
		return nil, sarama.ErrIncompleteResponse
	}

	block := response.GetBlock(ms.id.topic, ms.id.partition)
	if block == nil {
		return nil, sarama.ErrIncompleteResponse
	}

	if block.Err != sarama.ErrNoError {
		return nil, block.Err
	}

	if len(block.MsgSet.Messages) == 0 {
		// We got no messages. If we got a trailing one then we need to ask for more data.
		// Otherwise we just poll again and wait for one to be produced...
		if block.MsgSet.PartialTrailingMessage {
			if ms.f.saramaCfg.Consumer.Fetch.Max > 0 && ms.fetchSize == ms.f.saramaCfg.Consumer.Fetch.Max {
				// we can't ask for more data, we've hit the configured limit
				log.Infof("<%s> oversized message skipped: offset=%d", cid, ms.offset)
				ms.reportError(sarama.ErrMessageTooLarge)
				ms.offset++ // skip this one so we can keep processing future messages
			} else {
				ms.fetchSize *= 2
				if ms.f.saramaCfg.Consumer.Fetch.Max > 0 && ms.fetchSize > ms.f.saramaCfg.Consumer.Fetch.Max {
					ms.fetchSize = ms.f.saramaCfg.Consumer.Fetch.Max
				}
			}
		}

		return nil, nil
	}

	// we got messages, reset our fetch size in case it was increased for a previous request
	ms.fetchSize = ms.f.saramaCfg.Consumer.Fetch.Default
	var fetchedMessages []*consumer.Message
	for _, msgBlock := range block.MsgSet.Messages {
		for _, msg := range msgBlock.Messages() {
			if msg.Offset < ms.offset {
				continue
			}
			consumerMessage := &consumer.Message{
				Topic:         ms.id.topic,
				Partition:     ms.id.partition,
				Key:           msg.Msg.Key,
				Value:         msg.Msg.Value,
				Offset:        msg.Offset,
				HighWaterMark: block.HighWaterMarkOffset,
			}
			fetchedMessages = append(fetchedMessages, consumerMessage)
			ms.lag = block.HighWaterMarkOffset - msg.Offset
		}
	}

	if len(fetchedMessages) == 0 {
		return nil, sarama.ErrIncompleteResponse
	}
	return fetchedMessages, nil
}

// reportError sends message fetch errors to the error channel if the user
// configured the message stream to do so via `Config.Consumer.Return.Errors`.
func (ms *msgStream) reportError(err error) {
	if !ms.f.saramaCfg.Consumer.Return.Errors {
		return
	}
	ce := &Err{
		Topic:     ms.id.topic,
		Partition: ms.id.partition,
		Err:       err,
	}
	select {
	case ms.errorsCh <- ce:
	default:
	}
}

func (ms *msgStream) String() string {
	return ms.actorID.String()
}

// brokerExecutor maintains a connection with a particular Kafka broker. It
// processes fetch requests from message stream instances and sends responses
// back. The dispatcher goroutine of the message stream factory is responsible
// for keeping a broker executor alive while it is assigned to at least one
// message stream instance.
//
// implements `mapper.Executor`.
type brokerExecutor struct {
	aggrActorID     *actor.ID
	execActorID     *actor.ID
	config          *sarama.Config
	conn            *sarama.Broker
	requestsCh      chan fetchReq
	batchRequestsCh chan []fetchReq
	wg              sync.WaitGroup
}

type fetchReq struct {
	Topic     string
	Partition int32
	Offset    int64
	MaxBytes  int32
	Lag       int64
	ReplyToCh chan<- fetchRes
}

type fetchRes struct {
	Response *sarama.FetchResponse
	Err      error
}

// implements `mapper.Executor`.
func (be *brokerExecutor) BrokerConn() *sarama.Broker {
	return be.conn
}

// implements `mapper.Executor`.
func (be *brokerExecutor) Stop() {
	close(be.requestsCh)
	be.wg.Wait()
}

// runAggregator collects fetch requests from message streams into batches
// while the request executor goroutine is busy processing the previous batch.
// As soon as the executor is done, a new batch is handed over to it.
func (be *brokerExecutor) runAggregator() {
	defer close(be.batchRequestsCh)

	var nilOrBatchRequestCh chan<- []fetchReq
	var batchRequest []fetchReq
	for {
		select {
		case fr, ok := <-be.requestsCh:
			if !ok {
				return
			}
			batchRequest = append(batchRequest, fr)
			nilOrBatchRequestCh = be.batchRequestsCh
		case nilOrBatchRequestCh <- batchRequest:
			batchRequest = nil
			// Disable batchRequestsCh until we have at least one fetch request.
			nilOrBatchRequestCh = nil
		}
	}
}

// runExecutor executes fetch request aggregated into batches by the aggregator
// goroutine of the broker executor.
func (be *brokerExecutor) runExecutor() {
	var lastErr error
	var lastErrTime time.Time
	for fetchRequests := range be.batchRequestsCh {
		// Reject consume requests for awhile after a connection failure to
		// allow the Kafka cluster some time to recuperate.
		if time.Now().UTC().Sub(lastErrTime) < be.config.Consumer.Retry.Backoff {
			for _, fr := range fetchRequests {
				fr.ReplyToCh <- fetchRes{nil, lastErr}
			}
			continue
		}
		// Make a batch fetch request for all hungry message streams.
		req := &sarama.FetchRequest{
			MinBytes:    be.config.Consumer.Fetch.Min,
			MaxWaitTime: int32(be.config.Consumer.MaxWaitTime / time.Millisecond),
		}
		for _, fr := range fetchRequests {
			req.AddBlock(fr.Topic, fr.Partition, fr.Offset, fr.MaxBytes)
		}
		var res *sarama.FetchResponse
		res, lastErr = be.conn.Fetch(req)
		if lastErr != nil {
			lastErrTime = time.Now().UTC()
			be.conn.Close()
			log.Infof("<%s> connection reset: err=(%s)", be.execActorID, lastErr)
		}
		// Fan the response out to the message streams.
		for _, fr := range fetchRequests {
			fr.ReplyToCh <- fetchRes{res, lastErr}
		}
	}
}

func (be *brokerExecutor) String() string {
	if be == nil {
		return "<nil>"
	}
	return be.aggrActorID.String()
}
