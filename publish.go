package rabbitmq

import (
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// DeliveryMode. Transient means higher throughput but messages will not be
// restored on broker restart. The delivery mode of publishings is unrelated
// to the durability of the queues they reside on. Transient messages will
// not be restored to durable queues, persistent messages will be restored to
// durable queues and lost on non-durable queues during server restart.
//
// This remains typed as uint8 to match Publishing.DeliveryMode. Other
// delivery modes specific to custom queue implementations are not enumerated
// here.
const (
	Transient  uint8 = amqp.Transient
	Persistent uint8 = amqp.Persistent
)

// Return captures a flattened struct of fields returned by the server when a
// Publishing is unable to be delivered either due to the `mandatory` flag set
// and no route found, or `immediate` flag set and no free consumer.
type Return struct {
	amqp.Return
}

// PublishOptions are used to control how data is published
type PublishOptions struct {
	Exchange string
	// Mandatory fails to publish if there are no queues
	// bound to the routing key
	Mandatory bool
	// Immediate fails to publish if there are no consumers
	// that can ack bound to the queue on the routing key
	Immediate   bool
	ContentType string
	// Transient or Persistent
	DeliveryMode uint8
	// Expiration time in ms that a message will expire from a queue.
	// See https://www.rabbitmq.com/ttl.html#per-message-ttl-in-publishers
	Expiration string
	Headers    Table
}

// WithPublishOptionsExchange returns a function that sets the exchange to publish to
func WithPublishOptionsExchange(exchange string) func(*PublishOptions) {
	return func(options *PublishOptions) {
		options.Exchange = exchange
	}
}

// WithPublishOptionsMandatory makes the publishing mandatory, which means when a queue is not
// bound to the routing key a message will be sent back on the returns channel for you to handle
func WithPublishOptionsMandatory(options *PublishOptions) {
	options.Mandatory = true
}

// WithPublishOptionsImmediate makes the publishing immediate, which means when a consumer is not available
// to immediately handle the new message, a message will be sent back on the returns channel for you to handle
func WithPublishOptionsImmediate(options *PublishOptions) {
	options.Immediate = true
}

// WithPublishOptionsContentType returns a function that sets the content type, i.e. "application/json"
func WithPublishOptionsContentType(contentType string) func(*PublishOptions) {
	return func(options *PublishOptions) {
		options.ContentType = contentType
	}
}

// WithPublishOptionsPersistentDelivery sets the message to persist. Transient messages will
// not be restored to durable queues, persistent messages will be restored to
// durable queues and lost on non-durable queues during server restart. By default publishings
// are transient
func WithPublishOptionsPersistentDelivery(options *PublishOptions) {
	options.DeliveryMode = Persistent
}

// WithPublishOptionsExpiration returns a function that sets the expiry/TTL of a message. As per RabbitMq spec, it must be a
// string value in milliseconds.
func WithPublishOptionsExpiration(expiration string) func(options *PublishOptions) {
	return func(options *PublishOptions) {
		options.Expiration = expiration
	}
}

// WithPublishOptionsHeaders returns a function that sets message header values, i.e. "msg-id"
func WithPublishOptionsHeaders(headers Table) func(*PublishOptions) {
	return func(options *PublishOptions) {
		options.Headers = headers
	}
}

// Publisher allows you to publish messages safely across an open connection
type Publisher struct {
	chManager *channelManager

	notifyReturnChan chan Return

	disablePublishDueToFlow    bool
	disablePublishDueToFlowMux *sync.RWMutex

	logger Logger
}

// PublisherOptions are used to describe a publisher's configuration.
// Logging set to true will enable the consumer to print to stdout
type PublisherOptions struct {
	Logging bool
	Logger  Logger
}

// WithPublisherOptionsLogging sets logging to true on the consumer options
func WithPublisherOptionsLogging(options *PublisherOptions) {
	options.Logging = true
	options.Logger = &stdLogger{}
}

// WithPublisherOptionsLogger sets logging to a custom interface.
// Use WithPublisherOptionsLogging to just log to stdout.
func WithPublisherOptionsLogger(log Logger) func(options *PublisherOptions) {
	return func(options *PublisherOptions) {
		options.Logging = true
		options.Logger = log
	}
}

// NewPublisher returns a new publisher with an open channel to the cluster.
// If you plan to enforce mandatory or immediate publishing, those failures will be reported
// on the channel of Returns that you should setup a listener on.
// Flow controls are automatically handled as they are sent from the server, and publishing
// will fail with an error when the server is requesting a slowdown
func NewPublisher(url string, config amqp.Config, optionFuncs ...func(*PublisherOptions)) (Publisher, error) {
	options := &PublisherOptions{}
	for _, optionFunc := range optionFuncs {
		optionFunc(options)
	}
	if options.Logger == nil {
		options.Logger = &noLogger{} // default no logging
	}

	chManager, err := newChannelManager(url, config, options.Logger)
	if err != nil {
		return Publisher{}, err
	}

	publisher := Publisher{
		chManager:                  chManager,
		disablePublishDueToFlow:    false,
		disablePublishDueToFlowMux: &sync.RWMutex{},
		logger:                     options.Logger,
		notifyReturnChan:           nil,
	}

	go publisher.startNotifyFlowHandler()

	// restart notifiers when cancel/close is triggered
	go func() {
		for err := range publisher.chManager.notifyCancelOrClose {
			publisher.logger.Printf("publish cancel/close handler triggered. err: %v", err)
			go publisher.startNotifyFlowHandler()
			if publisher.notifyReturnChan != nil {
				go publisher.startNotifyReturnHandler()
			}
		}
	}()

	return publisher, nil
}

// NotifyReturn registers a listener for basic.return methods.
// These can be sent from the server when a publish is undeliverable either from the mandatory or immediate flags.
func (publisher *Publisher) NotifyReturn() <-chan Return {
	publisher.notifyReturnChan = make(chan Return)
	go publisher.startNotifyReturnHandler()
	return publisher.notifyReturnChan
}

// Publish publishes the provided data to the given routing keys over the connection
func (publisher *Publisher) Publish(
	data []byte,
	routingKeys []string,
	optionFuncs ...func(*PublishOptions),
) error {
	publisher.disablePublishDueToFlowMux.RLock()
	if publisher.disablePublishDueToFlow {
		return fmt.Errorf("publishing blocked due to high flow on the server")
	}
	publisher.disablePublishDueToFlowMux.RUnlock()

	options := &PublishOptions{}
	for _, optionFunc := range optionFuncs {
		optionFunc(options)
	}
	if options.DeliveryMode == 0 {
		options.DeliveryMode = Transient
	}

	for _, routingKey := range routingKeys {
		var message = amqp.Publishing{}
		message.ContentType = options.ContentType
		message.DeliveryMode = options.DeliveryMode
		message.Body = data
		message.Headers = tableToAMQPTable(options.Headers)
		message.Expiration = options.Expiration

		// Actual publish.
		err := publisher.chManager.channel.Publish(
			options.Exchange,
			routingKey,
			options.Mandatory,
			options.Immediate,
			message,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// StopPublishing stops the publishing of messages.
// The publisher should be discarded as it's not safe for re-use
func (publisher Publisher) StopPublishing() {
	publisher.chManager.channel.Close()
	publisher.chManager.connection.Close()
}

func (publisher *Publisher) startNotifyFlowHandler() {
	notifyFlowChan := publisher.chManager.channel.NotifyFlow(make(chan bool))
	publisher.disablePublishDueToFlowMux.Lock()
	publisher.disablePublishDueToFlow = false
	publisher.disablePublishDueToFlowMux.Unlock()

	// Listeners for active=true flow control.  When true is sent to a listener,
	// publishing should pause until false is sent to listeners.
	for ok := range notifyFlowChan {
		publisher.disablePublishDueToFlowMux.Lock()
		if ok {
			publisher.logger.Printf("pausing publishing due to flow request from server")
			publisher.disablePublishDueToFlow = true
		} else {
			publisher.disablePublishDueToFlow = false
			publisher.logger.Printf("resuming publishing due to flow request from server")
		}
		publisher.disablePublishDueToFlowMux.Unlock()
	}
}

func (publisher *Publisher) startNotifyReturnHandler() {
	returnAMQPCh := publisher.chManager.channel.NotifyReturn(make(chan amqp.Return, 1))
	for ret := range returnAMQPCh {
		publisher.notifyReturnChan <- Return{ret}
	}
}
