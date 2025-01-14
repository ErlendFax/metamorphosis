package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	log "github.com/celerway/chainsaw"
	"github.com/celerway/metamorphosis/bridge/observability"
	is2 "github.com/matryer/is"
	"github.com/segmentio/kafka-go"
	logrus "github.com/sirupsen/logrus"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockWriter struct {
	mu         sync.Mutex
	storage    []kafka.Message
	failed     bool
	msgs       uint64
	writes     uint64
	deadlock   bool
	batchDelay time.Duration
	msgDelay   time.Duration
}

func (m *mockWriter) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// if deadlock, block until context is cancelled
	if m.deadlock {
		log.Warn("writer is deadlocked")
		<-ctx.Done()
	}
	time.Sleep(m.batchDelay + m.msgDelay*time.Duration(len(msgs)))
	if m.failed {
		return errors.New("storage is in a failed state")
	}
	if m.storage == nil {
		m.storage = make([]kafka.Message, 0)
	}
	l := uint64(len(msgs))
	log.Debugf("Writing %d messages to pretend kafka", l)
	m.storage = append(m.storage, msgs...)
	atomic.AddUint64(&m.msgs, l)
	atomic.AddUint64(&m.writes, 1)
	return nil
}

func (m *mockWriter) setDelay(batchDelay, msgDelay time.Duration) {
	log.Infof("Setting storage delay to %v for batch / %v for msg", batchDelay, msgDelay)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchDelay = batchDelay
	m.msgDelay = msgDelay
}
func (m *mockWriter) setState(failed bool) {
	log.Info("Setting storage failed state to ", failed)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = failed
}
func (m *mockWriter) setDeadlock(deadlock bool) {
	log.Info("Setting storage deadlock to ", deadlock)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadlock = deadlock
}

func (m *mockWriter) getMessage(id int) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id >= len(m.storage) {
		return Message{}, errors.New("message not found")
	}
	var Msg Message
	err := json.Unmarshal(m.storage[id].Value, &Msg)
	if err != nil {
		return Message{}, err
	}
	return Msg, nil
}

func (m *mockWriter) getDecodedMessage(id int) (Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id >= len(m.storage) {
		return Message{}, errors.New("message not found")
	}
	mess := m.storage[id]
	var Msg Message
	err := json.Unmarshal(mess.Value, &Msg)
	if err != nil {
		return Message{}, err
	}
	return Msg, nil

}

func waitForAtomic(a *uint64, v uint64, timeout, sleeptime time.Duration) error {
	start := time.Now()
	for time.Since(start) < timeout {
		if atomic.LoadUint64(a) >= v {
			return nil
		}
		time.Sleep(sleeptime)
	}
	return fmt.Errorf("waitForAtomic (waiting for %d, is %d) timed out after %v", v, atomic.LoadUint64(a), timeout)
}

func TestMain(m *testing.M) {
	log.SetLevel(log.InfoLevel)
	log.Debug("Running test suite")
	ret := m.Run()
	log.Debug("Test suite complete")
	os.Exit(ret)
}

// Test that we can start and stop a buffer.
func TestBuffer_Run(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	storage := &mockWriter{}
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	log.Debug("Cancel issued. Waiting.")
	wg.Wait()
	log.Debug("Done")
}

func makeTestBuffer(writer *mockWriter) buffer {
	obsChannel := make(observability.Channel)
	go func() { // service the obs channel.
		for range obsChannel {
		}
	}()
	return buffer{
		interval:             2 * time.Millisecond,
		failureRetryInterval: 200 * time.Millisecond,
		buffer:               make([]kafka.Message, 0, 10),
		topic:                "unittest",
		writer:               writer,
		C:                    make(chan Message),
		batchSize:            5,
		maxBatchSize:         20,
		kafkaTimeout:         25 * time.Millisecond,
		logger:               logrus.WithFields(logrus.Fields{"module": "kafka", "instance": "test"}),
		obsChannel:           obsChannel,
		testMessageTopic:     "test",
	}
}

// Simple test. Send 10 messages and check that they are all received.
func TestBuffer_Process_ok(t *testing.T) {
	is := is2.New(t)
	storage := &mockWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()

	for i := 0; i < 10; i++ {
		buffer.C <- makeMessage("test", i)
	}
	cancel()
	wg.Wait()
	for i := 1; i <= 10; i++ {
		m, err := storage.getDecodedMessage(i)
		is.NoErr(err)
		is.Equal([]byte(fmt.Sprintf("%d", i-1)), m.Content)
	}
	log.Debug("Done")
}

// Somewhat more advanced. Induce a failure and check that the buffer recovers.
func TestBuffer_Process_fail(t *testing.T) {
	storage := &mockWriter{}
	ctx, cancel := context.WithCancel(context.Background())
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	log.Info("Sending msgs 0 -> 5 ")
	for i := 0; i < 5; i++ {
		buffer.C <- makeMessage("test", i)
	}
	storage.setState(true)
	log.Info("Sending msgs 5 -> 10")
	for i := 5; i < 10; i++ {
		buffer.C <- makeMessage("test", i)
	}
	log.Info("Done with msgs")
	time.Sleep(100 * time.Millisecond)
	storage.setState(false)
	time.Sleep(1 * time.Second)
	cancel()
	wg.Wait()
	for i := 0; i < 10; i++ {
		m, err := storage.getMessage(i)
		if err != nil {
			t.Errorf("Error getting message %d: %s", i, err)
		}
		fmt.Printf("Message: %s\n", string(m.Content))
	}
	log.Debug("Done")
}

// Test with the buffer in a failed state at startup.
func TestBuffer_Process_initial_fail(t *testing.T) {
	is := is2.New(t)
	storage := &mockWriter{}
	storage.setState(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	err := buffer.Run(ctx)
	is.True(err != nil) // should be error
	fmt.Println("Expected error: ", err)
}

// Test with the buffer deadlocking
func TestBuffer_deadlock(t *testing.T) {
	is := is2.New(t)
	is.True(true)
	storage := &mockWriter{}
	storage.setDeadlock(true)
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	wg := sync.WaitGroup{}
	wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	for i := 0; i < 50; i++ {
		buffer.C <- makeMessage("test", i)
	}
	time.Sleep(time.Millisecond * 100)
	cancel() // release the deadlock.
	time.Sleep(time.Millisecond * 100)
	for i := 0; i < 50; i++ {
		_, err := storage.getMessage(i)
		if err != nil {
			t.Errorf("Error getting message %d: %s", i, err)
		}
		/*
			topic := m.Topic
			body := m.Content
			fmt.Printf("Topic: %s Message: %s\n", topic, body)
		*/
	}

}

// TestBuffer_Process_slow - Induces slowness into the writer.
// It guards against re-ordering of the messages
func TestBuffer_Process_slow(t *testing.T) {
	const noOfMessages = 500
	is := is2.New(t)
	storage := &mockWriter{}

	storage.setDelay(2*time.Millisecond, time.Microsecond*20)
	ctx, cancel := context.WithCancel(context.Background())
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	for i := 0; i < noOfMessages; i++ {
		buffer.C <- makeMessage("test", i)
		time.Sleep(time.Microsecond * 10)
	}
	log.Info("Messages are sent")
	err := waitForAtomic(&storage.msgs, noOfMessages+1, time.Millisecond*5000, time.Millisecond)
	if err != nil {
		dumpLogs()
		t.Errorf("Error %s", err)
	}
	cancel()
	wg.Wait()
	for i := 1; i < noOfMessages; i++ {
		m, err := storage.getMessage(i)
		is.NoErr(err)
		is.Equal(fmt.Sprintf("%d", i-1), string(m.Content))
		is.Equal("test", m.Topic)
	}
	is.Equal(storage.msgs, uint64(noOfMessages+1))
	fmt.Println("==== Done ==== ")
	fmt.Printf("Writes %d ", storage.writes)
	fmt.Printf("Messages %d", storage.msgs)
	fmt.Println("\n =========== ")
}

func TestBuffer_Batching(t *testing.T) {
	const batchSize = 100
	const totalMsgs = 10000

	storage := &mockWriter{}
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	buffer.batchSize = batchSize
	buffer.maxBatchSize = 1000
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	storage.setState(false)
	for i := 0; i < totalMsgs; i++ {
		buffer.C <- makeMessage("test", i)
	}
	storage.setState(false)
	start := time.Now()
	err := waitForAtomic(&storage.msgs, totalMsgs+1, time.Millisecond*500, time.Millisecond)
	dur := time.Since(start)
	log.Info("Duration: ", dur)
	if err != nil {
		dumpLogs()
		t.Errorf("waitForAtomic Error %s", err)
	}
	if atomic.LoadUint64(&storage.writes) != batchSize+1 {
		dumpLogs()
		t.Errorf("Wrong number of batched writes: %d", atomic.LoadUint64(&storage.writes))
	}
	if atomic.LoadUint64(&storage.msgs) != totalMsgs+1 {
		dumpLogs()
		t.Errorf("Wrong number of messages: %d", atomic.LoadUint64(&storage.msgs))
	}
	cancel()
	wg.Wait()
	log.Debug("Done")

}

// Get the buffer up and running. Fails it and then proceeed to rewrite
// 10000 messages to it. See if it recovers and clears all the messages.
func TestBuffer_Batching_Recovery(t *testing.T) {
	is := is2.New(t)
	storage := &mockWriter{}
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	buffer.batchSize = 100
	buffer.maxBatchSize = 1000
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	storage.setState(false)
	buffer.C <- makeMessage("test", 0)
	storage.setState(true)
	for i := 0; i < 10000; i++ {
		buffer.C <- makeMessage("test", i)
	}
	storage.setState(false)
	err := waitForAtomic(&storage.msgs, 10002, time.Millisecond*2000, time.Millisecond)
	log.Infof("Writes: %d", atomic.LoadUint64(&storage.writes))
	log.Infof("Msgs: %d", atomic.LoadUint64(&storage.msgs))
	log.Infof("Failures: %d", buffer.failures)
	is.NoErr(err)
	is.Equal(atomic.LoadUint64(&storage.msgs), uint64(10002))
	is.Equal(atomic.LoadUint64(&storage.writes), uint64(12))
	is.Equal(buffer.failures, 1) // We expect one failure here.

	cancel()
	wg.Wait()
	log.Debug("Done")

}

// Pump a 1000 messages into the buffer when storage is failed.
// Have the storage recover.
// Interrrupt the recovery by failing the storage in the middle of the recovery.
// Then have the storage recover again
// Finally check that all messages have been written to the storage correctly in the right order.
func TestBuffer_Batching_RecoveryInterrupted(t *testing.T) {
	const count = 1000
	is := is2.New(t)
	storage := &mockWriter{}
	buffer := makeTestBuffer(storage)
	defer close(buffer.obsChannel)
	buffer.batchSize = 10
	buffer.maxBatchSize = 100
	storage.batchDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := buffer.Run(ctx)
		if err != nil {
			log.Errorf("Error %s", err)
		}
		log.Info("buffer run complete")
	}()
	time.Sleep(10 * time.Millisecond)
	storage.setState(true)
	for i := 1; i <= count; i++ {
		buffer.C <- makeMessage("test", i)
	}
	log.Info("Pumped 1000 messages into buffer")
	storage.setState(false)
	err := waitForAtomic(&storage.msgs, 500, time.Second*3, time.Nanosecond*100)
	is.NoErr(err)
	storage.setState(true)
	time.Sleep(100 * time.Millisecond)
	storage.setState(false)
	err = waitForAtomic(&storage.msgs, 1000, time.Second*3, time.Nanosecond*100)
	is.NoErr(err)
	log.Infof("Writes: %d", atomic.LoadUint64(&storage.writes))
	log.Infof("Msgs: %d", atomic.LoadUint64(&storage.msgs))
	log.Infof("Failures: %d", buffer.failures)
	cancel()
	wg.Wait()
	for i := 1; i <= count; i++ {
		msg := storage.storage[i]
		val := msg.Value
		jmsg := Message{}
		err := json.Unmarshal(val, &jmsg)
		is.NoErr(err)
		is.Equal(fmt.Sprintf("%d", i), string(jmsg.Content))
	}

	log.Debug("Done")

}

func makeMessage(topic string, id int) Message {
	return Message{
		Topic:   topic,
		Content: []byte(fmt.Sprintf("%d", id)),
	}
}

func dumpLogs() {
	fmt.Println("====== dumping logs ======")
	msgs := log.GetMessages(log.TraceLevel)
	for _, m := range msgs {
		fmt.Printf("%s: %s %s\n", m.LogLevel.String(), m.TimeStamp.Format(time.RFC3339), m.Message)
	}
	fmt.Println("====== end of dump ======")

}
