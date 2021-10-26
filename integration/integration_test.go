package integration

import (
	"bytes"
	"context"
	"fmt"
	"github.com/celerway/metamorphosis/bridge"
	"github.com/pingcap/failpoint"
	log "github.com/sirupsen/logrus"
	"math/rand"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

const noOfMessages = 20
const originMqttPort = 1883
const originKafkaPort = 9092
const defaultHealthPort = 8080

func TestMain(m *testing.M) {
	// Main setup goroutine
	f := log.TextFormatter{
		ForceColors:     true,
		FullTimestamp:   true,
		TimestampFormat: time.RFC3339Nano,
	}
	log.SetLevel(log.DebugLevel)

	log.SetFormatter(&f)
	log.Debug("Log level set")
	rand.Seed(time.Now().Unix())
	wg := sync.WaitGroup{}
	wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	startKafka(ctx, &wg)
	startMqtt(ctx)
	waitForKafka()
	ret := m.Run() // Run the tests.
	cancel()
	wg.Wait()
	os.Exit(ret)

}

func TestDummy(t *testing.T) {
	var stdout, stderr bytes.Buffer
	fmt.Println("Dummy test running. Listing topics.")
	cmd := exec.Command("rpk", "topic", "-v", "list")

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		t.Errorf("Kafka topic (list) stdout: %s stderr: %s err: %s", stdout.String(), stderr.String(), err)
	}
	log.Debugf("Stdout: %s, \nStderr: %s", stdout.String(), stderr.String())
}

/*
   	plan:
         - create a topic in Kafka (random name)
         - spin up the proxy
         - spin up bridge (remember to connect to the ports)
         - push X messages through the MQTT
         - see that they arrive in Kafka
         - verify obs data
*/
func RunBasic(t *testing.T) {

	fmt.Println("Testing basic stuff")
	rootCtx := context.Background()
	rTopic := getRandomString(12)
	kafkaTopic(t, "create", rTopic)
	bridgeCtx, bridgeCancel := context.WithCancel(rootCtx)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go bridge.Run(bridgeCtx, &wg, mkBrigeParam(originMqttPort, originKafkaPort, defaultHealthPort, rTopic))
	waitForBridge(defaultHealthPort)
	publishMqttMessages(t, rTopic, 1, 0, originMqttPort) // Publish messages
	verifyKafkaMessages(t, rTopic, 1, originKafkaPort)   // Verify the messages.
	// +1 for the kafka messages. (There is a test message, you know)
	verifyObsdata(t, defaultHealthPort, 1, 1+1, 0, 0)
	bridgeCancel()
	kafkaTopic(t, "delete", rTopic)
	wg.Wait()
}

// TestBasic is the most basic test. It fires up the bridge and pushes a messages through it.
// Run it twice to make sure we shut down cleanly.
func TestBasic(t *testing.T) {
	RunBasic(t)
	RunBasic(t)
}

func TestKafkaFailure(t *testing.T) {
	fmt.Println("TestKafkaFailure")
	rootCtx := context.Background()
	rTopic := getRandomString(12)
	kafkaTopic(t, "create", rTopic)
	bridgeCtx, bridgeCancel := context.WithCancel(rootCtx)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go bridge.Run(bridgeCtx, &wg, mkBrigeParam(originMqttPort, originKafkaPort, defaultHealthPort, rTopic))
	waitForBridge(defaultHealthPort)
	publishMqttMessages(t, rTopic, noOfMessages, 0, originMqttPort) // Publish X messages
	time.Sleep(time.Second * 3)                                     // Give kafka time to write stuff.
	fmt.Println("==== Kafka DISABLED === ")
	// Enable failure. Each write will spend 700ms before failing.
	err := failpoint.Enable("github.com/celerway/metamorphosis/bridge/kafka/writeFailure", "return(true)")
	if err != nil {
		t.Errorf("Could not enable failpoint: %s", err)
	}
	// New batch of messages. Now kafka should be dead. note the offset.
	publishMqttMessages(t, rTopic, noOfMessages, noOfMessages, originMqttPort) // Publish 2nd batch of messages
	fmt.Println("==== Kafka RECOVERED === ")
	time.Sleep(time.Second)
	verifyKafkaDown(t, defaultHealthPort)
	err = failpoint.Disable("github.com/celerway/metamorphosis/bridge/kafka/writeFailure")
	if err != nil {
		t.Errorf("Could not enable failpoint: %s", err)
	}
	fmt.Println("==== Kafka SLOWED === ")
	// Slow down kafka to X ms per write.
	err = failpoint.Enable("github.com/celerway/metamorphosis/bridge/kafka/writeDelay", "return(50)")
	if err != nil {
		t.Errorf("Could not enable failpoint: %s", err)
	}
	publishMqttMessages(t, rTopic, noOfMessages, noOfMessages*2, originMqttPort) // Publish 3rd batch of messages
	time.Sleep(3 * time.Second)                                                  // Give it some time to write messages.
	verifyKafkaMessages(t, rTopic, noOfMessages*3, originKafkaPort)              // Verify the messages.

	err = failpoint.Disable("github.com/celerway/metamorphosis/bridge/kafka/writeDelay")
	if err != nil {
		t.Errorf("Could not enable failpoint: %s", err)
	}
	fmt.Println("==== Kafka Good === ")

	fmt.Println("Done verifying kafka data. Checking obs data.")
	verifyObsdata(t, defaultHealthPort, noOfMessages*3, noOfMessages*3+2, 0, 1)
	bridgeCancel()
	kafkaTopic(t, "delete", rTopic)
	wg.Wait()
}

func TestMqttFailure(t *testing.T) {
	fmt.Println("Testing MQTT failure")
	rTopic := getRandomString(12)
	kafkaTopic(t, "create", rTopic)
	time.Sleep(time.Second)
	rootCtx := context.Background()
	bridgeCtx, bridgeCancel := context.WithCancel(rootCtx)
	wg := sync.WaitGroup{}
	wg.Add(1)
	go bridge.Run(bridgeCtx, &wg, mkBrigeParam(originMqttPort, originKafkaPort, defaultHealthPort, rTopic))
	waitForBridge(defaultHealthPort)

	publishMqttMessages(t, rTopic, noOfMessages, 0, originMqttPort) // Publish X messages
	restartMqtt()
	time.Sleep(300 * time.Millisecond)                                         // Give the bridge some time to reconnect, so we're sure we don't lose messages.
	publishMqttMessages(t, rTopic, noOfMessages, noOfMessages, originMqttPort) // Publish X messages
	verifyKafkaMessages(t, rTopic, noOfMessages*2, originKafkaPort)            // Verify the messages.
	bridgeCancel()
	wg.Wait()
	kafkaTopic(t, "delete", rTopic)
}
