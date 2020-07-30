package runsqs

import (
	"context"
	"math"
	"sync"
	"time"

	logger "github.com/asecurityteam/logevent"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
)

var mutex = &sync.Mutex{}

// DefaultSQSQueueConsumer is a naive implementation of an SQSConsumer.
// This implementation has no support for retries on nonpermanent failures;
// the result of every message consumption is followed by a deletion of
// the message. Furthermore, this implementation does not support concurrent
// processing of messages; messages are processed sequentially.
type DefaultSQSQueueConsumer struct {
	Queue           sqsiface.SQSAPI
	Logger          logger.Logger
	QueueURL        string
	deactivate      chan bool
	MessageConsumer SQSMessageConsumer
}

// StartConsuming starts consuming from the configured SQS queue
func (m *DefaultSQSQueueConsumer) StartConsuming(ctx context.Context) error {

	mutex.Lock()
	m.deactivate = make(chan bool)
	mutex.Unlock()

	var done = ctx.Done()
	for {
		select {
		case <-done:
			return nil
		case <-m.deactivate:
			return nil
		default:
		}
		var result, e = m.Queue.ReceiveMessage(&sqs.ReceiveMessageInput{
			QueueUrl: aws.String(m.QueueURL),
			AttributeNames: aws.StringSlice([]string{
				"SentTimestamp",
			}),
			MessageAttributeNames: aws.StringSlice([]string{
				"All",
			}),
			WaitTimeSeconds: aws.Int64(int64(math.Ceil((15 * time.Second).Seconds()))),
		})
		if e != nil {
			if !(request.IsErrorRetryable(e) || request.IsErrorThrottle(e)) {
				m.Logger.Error(e.Error())
			}
			time.Sleep(1 * time.Second)
			continue
		}
		for _, message := range result.Messages {
			_ = m.GetSQSMessageConsumer().ConsumeMessage(ctx, []byte(*message.Body))
			m.ackMessage(ctx, func() error {
				var _, e = m.Queue.DeleteMessage(&sqs.DeleteMessageInput{
					QueueUrl:      aws.String(m.QueueURL),
					ReceiptHandle: message.ReceiptHandle,
				})
				return e
			})
		}
		time.Sleep(time.Duration(1) * time.Millisecond)
	}
}

// StopConsuming stops this DefaultSQSQueueConsumer consuming from the SQS queue
func (m *DefaultSQSQueueConsumer) StopConsuming(ctx context.Context) error {
	mutex.Lock()
	if m.deactivate != nil {
		close(m.deactivate)
	}
	mutex.Unlock()
	return nil
}

// GetSQSMessageConsumer returns the MessageConsumer field. This function implies that
// DefaultSQSQueueConsumer MUST have a MessageConsumer defined.
func (m *DefaultSQSQueueConsumer) GetSQSMessageConsumer() SQSMessageConsumer {
	return m.MessageConsumer
}

func (m *DefaultSQSQueueConsumer) ackMessage(ctx context.Context, ack func() error) {
	for {
		e := ack()
		if e != nil {
			if !(request.IsErrorRetryable(e) || request.IsErrorThrottle(e)) {
				m.Logger.Error(e.Error())
				break
			}
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
}

// SmartSQSConsumer is an implementation of an SQSConsumer.
// This implementation supports...
// - retryable and non-retryable errors.
// - concurrent workers
type SmartSQSConsumer struct {
	Queue           sqsiface.SQSAPI
	Logger          logger.Logger
	QueueURL        string
	deactivate      chan bool
	MessageConsumer SQSMessageConsumer
	NumWorkers      uint64
	MessagePoolSize uint64
}

// StartConsuming starts consuming from the configured SQS queue
func (m *SmartSQSConsumer) StartConsuming(ctx context.Context) error {

	mutex.Lock()
	m.deactivate = make(chan bool)
	// messagePool represents a queue of messages that are waiting to be consumed
	messagePool := make(chan *sqs.Message, m.MessagePoolSize)

	// initialize all workers, pass in the pool of messages for each worker
	// to consume from
	for i := uint64(0); i < m.NumWorkers; i++ {
		go m.worker(ctx, messagePool)
	}
	mutex.Unlock()
	var done = ctx.Done()
	for {
		select {
		case <-done:
			// these close statements will cause all workers to eventually terminate
			close(messagePool)
			return nil
		case <-m.deactivate:
			close(messagePool)
			return nil
		default:
		}
		var result, e = m.Queue.ReceiveMessage(&sqs.ReceiveMessageInput{
			QueueUrl: aws.String(m.QueueURL),
			AttributeNames: aws.StringSlice([]string{
				"SentTimestamp",
			}),
			MessageAttributeNames: aws.StringSlice([]string{
				"All",
			}),
			WaitTimeSeconds: aws.Int64(int64(math.Ceil((15 * time.Second).Seconds()))),
		})
		if e != nil {
			if !(request.IsErrorRetryable(e) || request.IsErrorThrottle(e)) {
				m.Logger.Error(e.Error())
			}
			time.Sleep(1 * time.Second)
			continue
		}
		// loop through every message, and queue each message onto messagePool.
		// Because messagePool is a fixed buffered channel, there is potential for this to block.
		// It's important to set MessagePoolSize to a high enough size to account for high sqs throughput
		for _, message := range result.Messages {
			messagePool <- message
		}
		time.Sleep(time.Duration(1) * time.Millisecond)
	}
}

// worker function represents a single "message worker." worker will infinitely process messages until
// messages is closed. worker is responsible for handling deletion of messages, or handling
// messages that have retryable error.
func (m *SmartSQSConsumer) worker(ctx context.Context, messages <-chan *sqs.Message) {
	for message := range messages {
		err := m.GetSQSMessageConsumer().ConsumeMessage(ctx, []byte(*message.Body))
		if err != nil {
			switch err.(type) {
			case RetryableConsumerError:
				retryableErr := err.(RetryableConsumerError)
				m.ackMessage(ctx, func() error {
					var _, e = m.Queue.ChangeMessageVisibility(&sqs.ChangeMessageVisibilityInput{
						QueueUrl:          aws.String(m.QueueURL),
						ReceiptHandle:     message.ReceiptHandle,
						VisibilityTimeout: &retryableErr.VisibilityTimeout,
					})
					return e
				})

			default:
				m.ackMessage(ctx, func() error {
					var _, e = m.Queue.DeleteMessage(&sqs.DeleteMessageInput{
						QueueUrl:      aws.String(m.QueueURL),
						ReceiptHandle: message.ReceiptHandle,
					})
					return e
				})

			}
		}
		m.ackMessage(ctx, func() error {
			var _, e = m.Queue.DeleteMessage(&sqs.DeleteMessageInput{
				QueueUrl:      aws.String(m.QueueURL),
				ReceiptHandle: message.ReceiptHandle,
			})
			return e
		})
	}
}

// GetSQSMessageConsumer returns the MessageConsumer field. This function implies that
// DefaultSQSQueueConsumer MUST have a MessageConsumer defined.
func (m *SmartSQSConsumer) GetSQSMessageConsumer() SQSMessageConsumer {
	return m.MessageConsumer
}

func (m *SmartSQSConsumer) ackMessage(ctx context.Context, ack func() error) {
	for {
		e := ack()
		if e != nil {
			if !(request.IsErrorRetryable(e) || request.IsErrorThrottle(e)) {
				m.Logger.Error(e.Error())
				break
			}
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
}

// StopConsuming stops this DefaultSQSQueueConsumer consuming from the SQS queue
func (m *SmartSQSConsumer) StopConsuming(ctx context.Context) error {
	mutex.Lock()
	if m.deactivate != nil {
		close(m.deactivate)
	}
	mutex.Unlock()
	return nil
}