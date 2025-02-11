package mqtt

import (
	"context"
	"fmt"
	"github.com/celerway/metamorphosis/bridge/observability"
	paho "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"os"
	"time"
)

func Run(ctx context.Context, params Params) {
	client := client{
		broker:     params.Broker,
		port:       params.Port,
		topic:      params.Topic,
		clientId:   params.Clientid,
		tls:        params.Tls,
		ch:         params.Channel,
		obsChannel: params.ObsChannel,
		logger:     log.WithFields(log.Fields{"module": "mqtt"}),
	}
	client.logger.Debugf("Starting MQTT Worker.")
	client.logger.Debugf("Broker: %s:%d (tls: %v)", params.Broker, params.Port, params.Tls)

	opts := paho.NewClientOptions()
	if params.Tls {
		opts.SetTLSConfig(params.TlsConfig)
		opts.AddBroker(fmt.Sprintf("ssl://%s:%d", params.Broker, params.Port))
		client.tls = true
	} else {
		opts.AddBroker(fmt.Sprintf("mqtt://%s:%d", params.Broker, params.Port))
	}
	// opts.ResumeSubs = true
	opts.SetClientID(client.clientId)
	opts.SetConnectionLostHandler(client.handleDisconnect)
	opts.SetOnConnectHandler(client.handleConnect)
	pahoClient := paho.NewClient(opts)
	client.paho = pahoClient
	client.connect() // blocks and aborts on failure.
	client.logger.Info("Starting MQTT client worker")
	client.mainloop(ctx)
}

// mainloop
// This is a goroutine. When you return it dies.
// Connects to the MQTT broker, subscribes and processes messages.
// All the works happens in the event handler.
func (client *client) mainloop(ctx context.Context) {
	// Here we start blocking the goroutine and wait for shutdown.
	// If we need to keep track of something we can wrap this in a loop
	<-ctx.Done()
	client.logger.Info("MQTT client context is cancelled. Shutting down.")
	client.unsubscribe()
	client.paho.Disconnect(100)
	client.logger.Info("MQTT client exiting")
}

// connect to the broker. Blocks until the connection is established and the subscription is done.
func (client *client) connect() {
	const connectionAttempts = 10
	fullyConnected := false
	attempts := 0
	for !fullyConnected && attempts < connectionAttempts {
		client.logger.Infof("MQTT client connection attempt %d: connect to broker %s:%d (tls: %t).",
			attempts, client.broker, client.port, client.tls)
		if !client.paho.IsConnected() {
			client.logger.Info("MQTT client is not connected. Connecting.")
			token := client.paho.Connect()
			if token.Wait() && token.Error() != nil {
				// didn't connect. increase attempts and try again.
				client.logger.Errorf("Could not connect to MQTT (%s). ", token.Error())
				time.Sleep(200 * time.Millisecond) // Add some time so the broker isn't rushed by reconnects.
				attempts++
				continue
			}
			client.logger.Info("MQTT client connected. Will attempt subscribe.")
		}
		// connection should be up here. Issuing a MQTT subscribe now.
		err := client.subscribe() //  blocks. Also sets up handlers.
		if err != nil {
			client.logger.Errorf("Could not subscribe to topic '%s': %s", client.topic, err)
			time.Sleep(200 * time.Millisecond)
			attempts++
			continue
		}
		fullyConnected = true
	}
	if !fullyConnected && attempts >= connectionAttempts {
		client.logger.Errorf("Max number of connection attempt reached. Giving up and aborting.")
		os.Exit(1)
	}
	client.logger.Infof("Worker '%v' connected to MQTT %s:%d", client.clientId, client.broker, client.port)
}
func (client *client) handleConnect(_ paho.Client) {
	client.logger.Info("Connection to MQTT broker established")
}

func (client *client) handleDisconnect(_ paho.Client, err error) {
	client.logger.Errorf("handleDisconnect invoked with error: %s", err)
	time.Sleep(100 * time.Millisecond) // Add some time so the broker isn't rushed by reconnects.
	client.logger.Info("Reconnecting to broker.")
	client.connect()
}

func (client *client) unsubscribe() {
	token := client.paho.Unsubscribe(client.topic)
	if token.Wait() && token.Error() != nil {
		client.logger.Errorf("Could not unsubscribe from %s:  %s", client.topic, token.Error())
	} else {
		client.logger.Infof("Unsubscribed from topic '%s'", client.topic)
	}
}
func (client *client) subscribe() error {
	client.logger.Tracef("Issuing subscribe to topic '%s'", client.topic)
	token := client.paho.Subscribe(client.topic, 1, client.messageHandler)
	res := token.Wait()
	// sToken, ok := token.(paho.SubscribeToken)
	client.logger.Tracef("token.Wait is now: %v", res)
	if token.Error() != nil {
		client.logger.Errorf("Could not subscribe to '%s':  %s", client.topic, token.Error())
		return fmt.Errorf("subscribe error: %w", token.Error())
	}

	client.logger.Infof("successfully subcribed to '%s", client.topic)
	return nil
}

func (client *client) messageHandler(_ paho.Client, msg paho.Message) {
	client.logger.Tracef("Got message on topic %s. Message: %s", msg.Topic(), string(msg.Payload()))
	chMsg := ChannelMessage{
		Topic:   msg.Topic(),
		Content: msg.Payload(),
	}
	client.ch <- chMsg
	client.obsChannel <- observability.MattReceived
}
