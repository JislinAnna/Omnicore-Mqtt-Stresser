package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type PayloadGenerator func(i int) string

func defaultPayloadGen() PayloadGenerator {
	return func(i int) string {
		return fmt.Sprintf("this is msg #%d!", i)
	}
}

func constantPayloadGenerator(payload string) PayloadGenerator {
	return func(i int) string {
		return payload
	}
}

func filePayloadGenerator(filepath string) PayloadGenerator {
	inputPath := strings.Replace(filepath, "@", "", 1)
	content, err := ioutil.ReadFile(inputPath)
	if err != nil {
		fmt.Printf("error reading payload file: %v\n", err)
		os.Exit(1)
	}
	return func(i int) string {
		return string(content)
	}
}

type Worker struct {
	tenant                     string
	subscriberClientIdTemplate string
	publisherClientIdTemplate  string
	topicNameTemplate          string
	WorkerId                   int
	BrokerUrl                  string
	Username                   string
	Password                   string
	SkipTLSVerification        bool
	NumberOfMessages           int
	PayloadGenerator           PayloadGenerator
	Timeout                    time.Duration
	Retained                   bool
	PublisherQoS               byte
	SubscriberQoS              byte
	CA                         []byte
	Cert                       []byte
	Key                        []byte
	PauseBetweenMessages       time.Duration
}

func setSkipTLS(o *mqtt.ClientOptions) {
	oldTLSCfg := o.TLSConfig
	if oldTLSCfg == nil {
		oldTLSCfg = &tls.Config{}
	}
	oldTLSCfg.InsecureSkipVerify = true
	o.SetTLSConfig(oldTLSCfg)
}

func NewTLSConfig(ca, certificate, privkey []byte) (*tls.Config, error) {
	// Import trusted certificates from CA
	certpool := x509.NewCertPool()
	ok := certpool.AppendCertsFromPEM(ca)

	if !ok {
		return nil, fmt.Errorf("CA is invalid")
	}

	// Import client certificate/key pair
	cert, err := tls.X509KeyPair(certificate, privkey)
	if err != nil {
		return nil, err
	}

	// Create tls.Config with desired tls properties
	return &tls.Config{
		// RootCAs = certs used to verify server cert.
		RootCAs: certpool,
		// ClientAuth = whether to request cert from server.
		// Since the server is set up for SSL, this happens
		// anyways.
		ClientAuth: tls.NoClientCert,
		// ClientCAs = certs used to validate client cert.
		ClientCAs: nil,
		// InsecureSkipVerify = verify that cert contents
		// match server. IP matches what is in cert etc.
		InsecureSkipVerify: false,
		// Certificates = list of certs client sends to server.
		Certificates: []tls.Certificate{cert},
	}, nil
}

var SubscribeHandlerCount uint64 = 0

func PrintCount() {
	fmt.Printf("SubscribeHandlerCount:%d\n", SubscribeHandlerCount)

}
func (w *Worker) Run(ctx context.Context) {
	verboseLogger.Printf("[%d] initializing\n", w.WorkerId)

	queue := make(chan [2]string)
	cid := w.WorkerId
	_ = randomSource.Int31()

	_, err := os.Hostname()
	if err != nil {
		panic(err)
	}

	topicName := fmt.Sprintf(w.topicNameTemplate, w.WorkerId)
	subscriberClientId := fmt.Sprintf(w.subscriberClientIdTemplate, w.WorkerId)
	publisherClientId := fmt.Sprintf(w.publisherClientIdTemplate, w.WorkerId)

	verboseLogger.Printf("[%d] topic=%s subscriberClientId=%s publisherClientId=%s\n", cid, topicName, subscriberClientId, publisherClientId)
	publisherOptions := mqtt.NewClientOptions().SetClientID(publisherClientId).SetUsername("unused").SetPassword(w.Password).AddBroker(w.BrokerUrl)

	subscriberOptions := mqtt.NewClientOptions().SetClientID(subscriberClientId).SetUsername("unused").SetPassword(w.Password).AddBroker(w.BrokerUrl)

	subscriberOptions.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
		atomic.AddUint64(&SubscribeHandlerCount, 1)
		queue <- [2]string{msg.Topic(), string(msg.Payload())}
	})

	if len(w.CA) > 0 || len(w.Key) > 0 {
		tlsConfig, err := NewTLSConfig(w.CA, w.Cert, w.Key)
		if err != nil {
			panic(err)
		}
		subscriberOptions.SetTLSConfig(tlsConfig)
		publisherOptions.SetTLSConfig(tlsConfig)
	}

	if w.SkipTLSVerification {
		setSkipTLS(publisherOptions)
		setSkipTLS(subscriberOptions)
	}

	// subscriber := mqtt.NewClient(subscriberOptions)

	// verboseLogger.Printf("[%d] connecting subscriber\n", w.WorkerId)
	// if token := subscriber.Connect(); token.WaitTimeout(w.Timeout) && token.Error() != nil {
	// 	resultChan <- Result{
	// 		WorkerId:     w.WorkerId,
	// 		Event:        ConnectFailedEvent,
	// 		Error:        true,
	// 		ErrorMessage: token.Error(),
	// 	}

	// 	return
	// }

	// defer func() {
	// 	verboseLogger.Printf("[%d] unsubscribe\n", w.WorkerId)

	// 	if token := subscriber.Unsubscribe(topicName); token.WaitTimeout(w.Timeout) && token.Error() != nil {
	// 		fmt.Printf("failed to unsubscribe: %v\n", token.Error())
	// 	}

	// 	subscriber.Disconnect(5)
	// }()

	// verboseLogger.Printf("[%d] subscribing to topic\n", w.WorkerId)
	// if token := subscriber.Subscribe(topicName, w.SubscriberQoS, nil); token.WaitTimeout(w.Timeout) && token.Error() != nil {
	// 	resultChan <- Result{
	// 		WorkerId:     w.WorkerId,
	// 		Event:        SubscribeFailedEvent,
	// 		Error:        true,
	// 		ErrorMessage: token.Error(),
	// 	}

	// 	return
	// }

	publisher := mqtt.NewClient(publisherOptions)
	verboseLogger.Printf("[%d] connecting publisher\n", w.WorkerId)
	if token := publisher.Connect(); token.WaitTimeout(w.Timeout) && token.Error() != nil {
		resultChan <- Result{
			WorkerId:     w.WorkerId,
			Event:        ConnectFailedEvent,
			Error:        true,
			ErrorMessage: token.Error(),
		}
		return
	}

	verboseLogger.Printf("[%d] starting control loop %s\n", w.WorkerId, topicName)

	receivedCount := 0
	publishedCount := 0

	t0 := time.Now()
	for i := 0; i < w.NumberOfMessages; i++ {
		text := w.PayloadGenerator(i)
		token := publisher.Publish(topicName, w.PublisherQoS, w.Retained, text)
		published := token.WaitTimeout(w.Timeout)
		if published {
			publishedCount++
		}
		time.Sleep(w.PauseBetweenMessages)
	}
	publisher.Disconnect(5)

	publishTime := time.Since(t0)
	verboseLogger.Printf("[%d] all messages published\n", w.WorkerId)
	resultChan <- Result{
		WorkerId:          w.WorkerId,
		Event:             CompletedEvent,
		PublishTime:       publishTime,
		ReceiveTime:       time.Since(t0),
		MessagesReceived:  receivedCount,
		MessagesPublished: publishedCount,
	}

	verboseLogger.Printf("[%d] worker finished\n", w.WorkerId)
}
