package runsqs

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	aws "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/sqs"
	gomock "github.com/golang/mock/gomock"
)

var queueURL = "http://awssomething.com"

var sqsInput = &sqs.ReceiveMessageInput{
	QueueUrl: aws.String(queueURL),
	AttributeNames: aws.StringSlice([]string{
		"SentTimestamp",
	}),
	MessageAttributeNames: aws.StringSlice([]string{
		"All",
	}),
	WaitTimeSeconds: aws.Int64(int64(math.Ceil((15 * time.Second).Seconds()))),
}

var sqsEmptyMessageOutput = &sqs.ReceiveMessageOutput{
	Messages: []*sqs.Message{},
}

var defaultSQSMessage = &sqs.Message{
	Body:          aws.String("body"),
	ReceiptHandle: aws.String("receipt"),
}

/********************************
TestDefaultSQSQueueConsumer TESTS
********************************/

// TestDefaultSQSQueueConsumer_GoldenPath tests whether it can retrieve 5 messages,
// process those 5 messages, and delete those 5 messages
func TestDefaultSQSQueueConsumer_GoldenPath(t *testing.T) {
	// mocks
	var ctrl = gomock.NewController(t)
	defer ctrl.Finish()
	mockQueue := NewMockSQSAPI(ctrl)
	mockLogger := NewMockLogger(ctrl)
	mockMessageConsumer := NewMockSQSMessageConsumer(ctrl)

	// testBlocker is used to make this test deterministic(avoid timeouts)
	var testBlocker sync.WaitGroup
	var consumer = &DefaultSQSQueueConsumer{
		Logger:          mockLogger,
		QueueURL:        queueURL,
		Queue:           mockQueue,
		MessageConsumer: mockMessageConsumer,
	}

	receiveMessageOutput := &sqs.ReceiveMessageOutput{
		Messages: []*sqs.Message{
			defaultSQSMessage,
		},
	}

	// the following mocks test for exactly 5 successful message consumptions, no more no less
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(receiveMessageOutput, nil).Times(5)
	mockMessageConsumer.EXPECT().ConsumeMessage(gomock.Any(), []byte(*defaultSQSMessage.Body)).Return(nil).Times(5)
	mockQueue.EXPECT().DeleteMessage(gomock.Any()).DoAndReturn(func(interface{}) (*sqs.DeleteMessageOutput, error) {
		testBlocker.Done()
		return nil, nil
	}).Times(5)

	// infinitely ping empty sqs
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(sqsEmptyMessageOutput, nil).AnyTimes()

	testBlocker.Add(5)
	go consumer.StartConsuming(context.Background())
	testBlocker.Wait()
	consumer.StopConsuming(context.Background())

}

// TestDefaultSQSQueueConsumer_ReceivingMessageFailure tests tests whether it can retrieve 2 messages, both of them fail,
// but only one is retryable. The non retryable causes a log.error
func TestDefaultSQSQueueConsumer_ReceivingMessageFailure(t *testing.T) {
	// mocks
	var ctrl = gomock.NewController(t)
	defer ctrl.Finish()
	mockQueue := NewMockSQSAPI(ctrl)
	mockLogger := NewMockLogger(ctrl)
	mockMessageConsumer := NewMockSQSMessageConsumer(ctrl)

	// testBlocker is used to make this test deterministic(avoid timeouts)
	var testBlocker sync.WaitGroup
	var consumer = &DefaultSQSQueueConsumer{
		Logger:          mockLogger,
		QueueURL:        queueURL,
		Queue:           mockQueue,
		MessageConsumer: mockMessageConsumer,
	}

	// 1 retryables, 1 error log
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(sqsEmptyMessageOutput, awserr.New("RequestThrottled", "test", nil))
	mockLogger.EXPECT().Error(gomock.Any()).Times(1)
	// non retryable
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(sqsEmptyMessageOutput, awserr.New("RequestCanceled", "test", nil))

	// infinitely ping empty sqs
	mockQueue.EXPECT().ReceiveMessage(sqsInput).DoAndReturn(func(interface{}) (interface{}, error) {
		defer testBlocker.Done()
		return sqsEmptyMessageOutput, nil
	}).AnyTimes()
	testBlocker.Add(1)
	go consumer.StartConsuming(context.Background())
	testBlocker.Wait()
	consumer.StopConsuming(context.Background())

}

/********************************
TestSmartSQSConsumer TESTS
********************************/

// TestSmartSQSConsumer_GoldenPath tests whether it can retrieve 5000 messages,
// process those 5000 messages concurrently, and delete those 5000 messages
func TestSmartSQSConsumer_GoldenPath(t *testing.T) {
	// mocks
	var ctrl = gomock.NewController(t)
	defer ctrl.Finish()
	mockQueue := NewMockSQSAPI(ctrl)
	mockLogger := NewMockLogger(ctrl)
	mockMessageConsumer := NewMockSQSMessageConsumer(ctrl)

	// testBlocker is used to make this test deterministic(avoid timeouts)
	var testBlocker sync.WaitGroup
	var consumer = &SmartSQSConsumer{
		Logger:          mockLogger,
		QueueURL:        queueURL,
		Queue:           mockQueue,
		MessageConsumer: mockMessageConsumer,
		NumWorkers:      10,
		MessagePoolSize: 100,
	}

	messages := []*sqs.Message{}
	for i := 0; i < 1000; i++ {
		messages = append(messages, defaultSQSMessage)
	}

	receiveMessageOutput := &sqs.ReceiveMessageOutput{
		Messages: messages,
	}
	// the following mocks test for exactly 5 successful message consumptions, no more no less
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(receiveMessageOutput, nil).Times(5)
	mockMessageConsumer.EXPECT().ConsumeMessage(gomock.Any(), []byte(*defaultSQSMessage.Body)).Return(nil).Times(5000)
	mockQueue.EXPECT().DeleteMessage(gomock.Any()).DoAndReturn(func(interface{}) (*sqs.DeleteMessageOutput, error) {
		testBlocker.Done()
		return nil, nil
	}).Times(5000)

	// infinitely ping empty sqs
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(sqsEmptyMessageOutput, nil).AnyTimes()

	testBlocker.Add(5000)
	go consumer.StartConsuming(context.Background())
	testBlocker.Wait()
	consumer.StopConsuming(context.Background())

}

// TestSmartSQSConsumer_ReceivingMessageFailure tests whether it can retrieve 2 messages, both of them fail,
// but only one is retryable. The non retryable causes a log.error
func TestSmartSQSConsumer_ReceivingMessageFailure(t *testing.T) {
	// mocks
	var ctrl = gomock.NewController(t)
	defer ctrl.Finish()
	mockQueue := NewMockSQSAPI(ctrl)
	mockLogger := NewMockLogger(ctrl)
	mockMessageConsumer := NewMockSQSMessageConsumer(ctrl)

	// testBlocker is used to make this test deterministic(avoid timeouts)
	var testBlocker sync.WaitGroup
	var consumer = &SmartSQSConsumer{
		Logger:          mockLogger,
		QueueURL:        queueURL,
		Queue:           mockQueue,
		MessageConsumer: mockMessageConsumer,
		NumWorkers:      10,
		MessagePoolSize: 100,
	}

	// 1 retryables, 1 error log
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(sqsEmptyMessageOutput, awserr.New("RequestThrottled", "test", nil))
	mockLogger.EXPECT().Error(gomock.Any()).Times(1)
	// non retryable
	mockQueue.EXPECT().ReceiveMessage(sqsInput).Return(sqsEmptyMessageOutput, awserr.New("RequestCanceled", "test", nil))

	// infinitely ping empty sqs
	mockQueue.EXPECT().ReceiveMessage(sqsInput).DoAndReturn(func(interface{}) (interface{}, error) {
		defer testBlocker.Done()
		return sqsEmptyMessageOutput, nil
	}).AnyTimes()
	testBlocker.Add(1)
	go consumer.StartConsuming(context.Background())
	testBlocker.Wait()
	consumer.StopConsuming(context.Background())

}