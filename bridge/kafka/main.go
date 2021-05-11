package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/celerway/metamorphosis/bridge/observability"
	gokafka "github.com/segmentio/kafka-go"
	log "github.com/sirupsen/logrus"
	"time"
)

// Run Constructor. Sort of.
func Run(ctx context.Context, params KafkaParams, id int) {
	// This should be fairly easy to test in case we wanna mock Kafka.
	client := kafkaClient{
		broker:        params.Broker,
		port:          params.Port,
		ch:            params.Channel,
		waitGroup:     params.WaitGroup,
		topic:         params.Topic,
		obsChannel:    params.ObsChannel,
		writeHandler:  handleMessageWrite,
		retryInterval: params.RetryInterval,
		logger: log.WithFields(log.Fields{
			"module": "kafka",
			"worker": fmt.Sprint(id),
		}),
	}
	client.writer = getWriter(client) // Give the writer the context aware logger and store it in the struct.

	// Sends a test message to Kafka. This will block Run so when Run returns we
	// know we're OK.
	if !sendTestMessage(ctx, client) {
		client.logger.Fatalf("Can't send test message on startup. Aborting.")
	}
	go mainloop(ctx, client)

}

func mainloop(ctx context.Context, client kafkaClient) {
	client.waitGroup.Add(1)
	keepRunning := true
	msgBuffer := make([]KafkaMessage, 0) // Buffer to store things if Kafka is causing issues.
	alive := true
	var lastAttempt time.Time
	client.logger.Infof("Kafka writer running %s:%d, retry is %v", client.broker, client.port, client.retryInterval)
	for keepRunning {
		select {
		case <-ctx.Done():
			client.logger.Info("Kafka writer shutting down")
			keepRunning = false
		case <-time.After(client.retryInterval): // Automatically retry even if there are no new messages.
			if !alive {
				success := sendTestMessage(ctx, client)
				if success {
					client.logger.Warnf("Kafka has recovered (retryInterval) Spool: %d", len(msgBuffer))
					lastAttempt = time.Now()
					msgBuffer, alive = despool(ctx, msgBuffer, client) // Actual de-spool here.
				}
			}
		case msg := <-client.ch: // Got a message from the bridge.
			if alive {
				success := client.writeHandler(ctx, client, msg) // Send msg.
				if !success {                                    // Kafka failed. :-(
					msgBuffer = append(msgBuffer, msg)
					client.logger.Infof("Message spooled. Currently %d messages in the spool.", len(msgBuffer))
					alive = false
					lastAttempt = time.Now() // Time of last failure.
				}
			} else { // alive == false here.
				if time.Since(lastAttempt) < client.retryInterval { // Less than Xs since last try. Just spool the message.
					msgBuffer = append(msgBuffer, msg) // Todo: Should we limit the number of messages we can spool?
					client.logger.Infof("Message spooled. Currently %d messages in the spool.", len(msgBuffer))
				} else { // retryInterval passed. Lets try a test message.
					success := sendTestMessage(ctx, client)
					if success {
						client.logger.Warnf("Kafka has recovered (on new message) Spool: %d", len(msgBuffer))
						lastAttempt = time.Now()
						msgBuffer, alive = despool(ctx, msgBuffer, client) // Actual de-spool here.
					} else { // success == false
						lastAttempt = time.Now()
						msgBuffer = append(msgBuffer, msg)
					}
				}
			}
		}
	}
	client.logger.Info("Kafka done.")
	client.waitGroup.Done()
}

// despool
// Returns buffer, alive
func despool(ctx context.Context, buffer []KafkaMessage, client kafkaClient) ([]KafkaMessage, bool) {
	successes := 0
	client.logger.Warnf("Will attempt de-spool %d messages", len(buffer))
	for i, msg := range buffer {
		client.logger.Debugf("Despooling trying to de-spool %d", i)
		success := client.writeHandler(ctx, client, msg)
		if success {
			successes++
			continue
		}
		client.logger.Errorf("Got an error while de-spooling. Succeeded with %d msgs. Rest is still spooled",
			successes)
		// Gosh darn it! Kafka is down again.
		// i should point at the last successful message we sent.
		// If we didn't send any i will be 0 and we'll return the whole slice.
		return buffer[i:], false
	}
	client.logger.Warnf("Successfully de-spooled %d messages", successes)
	// Return an empty slice.
	return []KafkaMessage{}, true
}

// This creates a write struct. Used when initializing.
func getWriter(client kafkaClient) *gokafka.Writer {
	broker := fmt.Sprintf("%s:%d",
		client.broker, client.port)
	w := &gokafka.Writer{
		Addr:         gokafka.TCP(broker),
		Topic:        client.topic,
		Balancer:     &gokafka.LeastBytes{},
		BatchSize:    1, // Write single messages.
		MaxAttempts:  1,
		RequiredAcks: gokafka.RequireAll,
		ErrorLogger:  client.logger,
	}
	client.logger.Debugf("Created a Kafka writer on %s/%s", broker, client.topic)
	return w
}

// The handler that gets called when we get a message.
func handleMessageWrite(ctx context.Context, client kafkaClient, msg KafkaMessage) bool {
	startWriteTime := time.Now()
	client.logger.Debugf("Issuing write to kafka (mqtt topic: %s)", msg.Topic)
	msgJson, err := json.Marshal(msg)
	if err != nil {
		client.logger.Errorf("Could not marshal message %v: %s", msg, err)
		client.obsChannel <- observability.KafkaError
		return true // Guess there isn't much we can do at this point but to move on.
	}
	client.logger.Tracef("Kafka(%s): %s", msg.Topic, string(msgJson))
	kMsg := gokafka.Message{Value: msgJson}
	err = client.writer.WriteMessages(ctx, kMsg)
	if err != nil {
		client.obsChannel <- observability.KafkaError
		client.logger.Errorf("Kafka: Error while writing: %s", err)
		return false
	} else {
		client.obsChannel <- observability.KafkaSent
	}
	client.logger.Debugf("Write done(topic %s). Took %v", msg.Topic, time.Since(startWriteTime))
	return true
}

// sendTestMessage sends a test message with the mqtt topic "test".
// You wanna ignore these messages in the Kafka consumers.
func sendTestMessage(ctx context.Context, client kafkaClient) bool {
	testMsg := KafkaMessage{
		Topic:   "test",
		Content: []byte("Just a test"),
	}
	return handleMessageWrite(ctx, client, testMsg)
}
