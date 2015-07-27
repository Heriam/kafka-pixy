package pixy

import (
	"fmt"
	"sync"
	"time"

	"github.com/mailgun/kafka-pixy/Godeps/_workspace/src/github.com/Shopify/sarama"
	"github.com/mailgun/kafka-pixy/Godeps/_workspace/src/github.com/mailgun/log"
)

const (
	chanBufferSize       = 128
	maxEncoderReprLength = 4096
)

// KafkaProducer builds on top of `sarama.AsyncProducer` to improve the
// shutdown handling. The problem it solves is that `sarama.AsyncProducer`
// drops all buffered messages as soon as it is ordered to shutdown. On the
// contrary, when `GracefulProducer` is ordered to stop it allows some time
// for the buffered messages to be committed to the Kafka cluster, and only
// when that time has elapsed it drops uncommitted messages.
type KafkaProducer struct {
	saramaClient    sarama.Client
	saramaProducer  sarama.AsyncProducer
	shutdownTimeout time.Duration
	deadMessageCh   chan<- *ProduceResult
	dispatcherCh    chan *sarama.ProducerMessage
	resultCh        chan *ProduceResult
	wg              sync.WaitGroup
}

type ProduceResult struct {
	Msg *sarama.ProducerMessage
	Err error
}

// SpawnKafkaProducer creates a `KafkaProducer` instance and starts its internal
// goroutines.
func SpawnKafkaProducer(saramaClient sarama.Client, shutdownTimeout time.Duration,
	deadMessageCh chan<- *ProduceResult) (*KafkaProducer, error) {

	saramaProducer, err := sarama.NewAsyncProducerFromClient(saramaClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kafka producer, cause=(%v)", err)
	}
	kp := &KafkaProducer{
		saramaClient:    saramaClient,
		saramaProducer:  saramaProducer,
		shutdownTimeout: shutdownTimeout,
		deadMessageCh:   deadMessageCh,
		dispatcherCh:    make(chan *sarama.ProducerMessage, chanBufferSize),
		resultCh:        make(chan *ProduceResult, chanBufferSize),
	}
	goGo(&kp.wg, kp.merger)
	goGo(&kp.wg, kp.dispatcher)
	return kp, nil
}

// Stop shuts down all producer goroutines and releases all resources.
func (kp *KafkaProducer) Stop() {
	close(kp.dispatcherCh)
	kp.wg.Wait()
}

func (kp *KafkaProducer) Produce(topic string, key, message sarama.Encoder) error {
	replyCh := make(chan error, 1)
	prodMsg := &sarama.ProducerMessage{
		Topic:    topic,
		Key:      key,
		Value:    message,
		Metadata: replyCh,
	}
	kp.dispatcherCh <- prodMsg
	return <-replyCh
}

func (kp *KafkaProducer) AsyncProduce(topic string, key, message sarama.Encoder) {
	prodMsg := &sarama.ProducerMessage{
		Topic: topic,
		Key:   key,
		Value: message,
	}
	kp.dispatcherCh <- prodMsg
}

// merger receives both message acknowledgements and producer errors from the
// respective `sarama.AsyncProducer` channels, constructs `ProducerResult`s out
// of them and sends the constructed `ProducerResult` instances to `resultCh`
// to be further inspected by the `dispatcher` goroutine.
//
// It keeps running until both `sarama.AsyncProducer` output channels are
// closed. Then it closes the `resultCh` to notify the `dispatcher` goroutine
// that all pending messages have been processed and exits.
func (kp *KafkaProducer) merger() {
	defer logScope("ProducerMerger")()
	nilOrProdSuccessesCh := kp.saramaProducer.Successes()
	nilOrProdErrorsCh := kp.saramaProducer.Errors()
mergeLoop:
	for channelsOpened := 2; channelsOpened > 0; {
		select {
		case ackedMsg, ok := <-nilOrProdSuccessesCh:
			if !ok {
				channelsOpened -= 1
				nilOrProdSuccessesCh = nil
				continue mergeLoop
			}
			kp.resultCh <- &ProduceResult{Msg: ackedMsg}
		case prodErr, ok := <-nilOrProdErrorsCh:
			if !ok {
				channelsOpened -= 1
				nilOrProdErrorsCh = nil
				continue mergeLoop
			}
			kp.resultCh <- &ProduceResult{Msg: prodErr.Msg, Err: prodErr.Err}
		}
	}
	// Close the result channel to notify the `dispatcher` goroutine that all
	// pending messages have been processed.
	close(kp.resultCh)
}

// dispatcher implements message processing and graceful shutdown. It receives
// messages from `dispatchedCh` where they are send to by `Produce` method and
// submits them to the embedded `sarama.AsyncProducer`. The dispatcher main
// purpose is to prevent loss of messages during shutdown. It achieves that by
// allowing some graceful period after it stops receiving messages and stopping
// the embedded `sarama.AsyncProducer`.
func (kp *KafkaProducer) dispatcher() {
	defer logScope("ProducerDispatcher")()
	nilOrDispatcherCh := kp.dispatcherCh
	var nilOrProdInputCh chan<- *sarama.ProducerMessage
	pendingMsgCount := 0
	// The normal operation loop is implemented as two-stroke machine. On the
	// first stroke a message is received from `dispatchCh`, and on the second
	// it is sent to `prodInputCh`. Note that producer results can be received
	// at any time.
	prodMsg := (*sarama.ProducerMessage)(nil)
	channelOpened := true
	for {
		select {
		case prodMsg, channelOpened = <-nilOrDispatcherCh:
			if !channelOpened {
				goto gracefulShutdown
			}
			pendingMsgCount += 1
			nilOrDispatcherCh = nil
			nilOrProdInputCh = kp.saramaProducer.Input()
		case nilOrProdInputCh <- prodMsg:
			nilOrDispatcherCh = kp.dispatcherCh
			nilOrProdInputCh = nil
		case prodResult := <-kp.resultCh:
			pendingMsgCount -= 1
			kp.handleProduceResult(prodResult)
		}
	}
gracefulShutdown:
	// Give the `sarama.AsyncProducer` some time to commit buffered messages.
	log.Infof("About to stop producer: pendingMsgCount=%d", pendingMsgCount)
	shutdownTimeoutCh := time.After(kp.shutdownTimeout)
	for pendingMsgCount > 0 {
		select {
		case <-shutdownTimeoutCh:
			goto shutdownNow
		case prodResult := <-kp.resultCh:
			pendingMsgCount -= 1
			kp.handleProduceResult(prodResult)
		}
	}
shutdownNow:
	log.Infof("Stopping producer: pendingMsgCount=%d", pendingMsgCount)
	kp.saramaProducer.AsyncClose()
	for prodResult := range kp.resultCh {
		kp.handleProduceResult(prodResult)
	}
}

// handleProduceResult inspects a production results and if it is an error
// then logs it and flushes it down the `deadMessageCh` if one had been
// configured.
func (kp *KafkaProducer) handleProduceResult(result *ProduceResult) {
	if replyCh, ok := result.Msg.Metadata.(chan error); ok {
		replyCh <- result.Err
	}
	if result.Err == nil {
		return
	}
	prodMsgRepr := fmt.Sprintf(`{Topic: "%s", Key: "%s", Value: "%s"}`,
		result.Msg.Topic, encoderRepr(result.Msg.Key), encoderRepr(result.Msg.Value))
	log.Errorf("Failed to submit message: msg=%v, cause=(%v)",
		prodMsgRepr, result.Err)
	if kp.deadMessageCh != nil {
		kp.deadMessageCh <- result
	}
}

// encoderRepr returns the string representation of an encoder value. The value
// is truncated to `maxEncoderReprLength`.
func encoderRepr(e sarama.Encoder) string {
	var repr string
	switch e := e.(type) {
	case sarama.StringEncoder:
		repr = string(e)
	case sarama.ByteEncoder:
		repr = fmt.Sprintf("%X", []byte(e))
	default:
		repr = fmt.Sprint(e)
	}
	if length := len(repr); length > maxEncoderReprLength {
		repr = fmt.Sprintf("%s... (%d bytes more)",
			repr[:maxEncoderReprLength], length-maxEncoderReprLength)
	}
	return repr
}