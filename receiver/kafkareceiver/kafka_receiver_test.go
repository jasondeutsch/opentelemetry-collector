// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//       http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kafkareceiver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Shopify/sarama"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opencensus.io/stats/view"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtls"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/exporter/kafkaexporter"
	"go.opentelemetry.io/collector/internal/testdata"
)

func TestNewTracesReceiver_version_err(t *testing.T) {
	c := Config{
		Encoding:        defaultEncoding,
		ProtocolVersion: "none",
	}
	r, err := newTracesReceiver(c, component.ReceiverCreateParams{}, defaultTracesUnmarshallers(), consumertest.NewNop())
	assert.Error(t, err)
	assert.Nil(t, r)
}

func TestNewTracesReceiver_encoding_err(t *testing.T) {
	c := Config{
		Encoding: "foo",
	}
	r, err := newTracesReceiver(c, component.ReceiverCreateParams{}, defaultTracesUnmarshallers(), consumertest.NewNop())
	require.Error(t, err)
	assert.Nil(t, r)
	assert.EqualError(t, err, errUnrecognizedEncoding.Error())
}

func TestNewTracesReceiver_err_auth_type(t *testing.T) {
	c := Config{
		ProtocolVersion: "2.0.0",
		Authentication: kafkaexporter.Authentication{
			TLS: &configtls.TLSClientSetting{
				TLSSetting: configtls.TLSSetting{
					CAFile: "/doesnotexist",
				},
			},
		},
		Encoding: defaultEncoding,
		Metadata: kafkaexporter.Metadata{
			Full: false,
		},
	}
	r, err := newTracesReceiver(c, component.ReceiverCreateParams{}, defaultTracesUnmarshallers(), consumertest.NewNop())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load TLS config")
	assert.Nil(t, r)
}

func TestTracesReceiverStart(t *testing.T) {
	testClient := testConsumerGroup{once: &sync.Once{}}
	c := kafkaTracesConsumer{
		nextConsumer:  consumertest.NewNop(),
		logger:        zap.NewNop(),
		consumerGroup: testClient,
	}

	err := c.Start(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, c.Shutdown(context.Background()))
}

func TestTracesReceiverStartConsume(t *testing.T) {
	testClient := testConsumerGroup{once: &sync.Once{}}
	c := kafkaTracesConsumer{
		nextConsumer:  consumertest.NewNop(),
		logger:        zap.NewNop(),
		consumerGroup: testClient,
	}
	ctx, cancelFunc := context.WithCancel(context.Background())
	c.cancelConsumeLoop = cancelFunc
	require.NoError(t, c.Shutdown(context.Background()))
	err := c.consumeLoop(ctx, &tracesConsumerGroupHandler{
		ready: make(chan bool),
	})
	assert.EqualError(t, err, context.Canceled.Error())
}

func TestTracesReceiver_error(t *testing.T) {
	zcore, logObserver := observer.New(zapcore.ErrorLevel)
	logger := zap.New(zcore)

	expectedErr := fmt.Errorf("handler error")
	testClient := testConsumerGroup{once: &sync.Once{}, err: expectedErr}
	c := kafkaTracesConsumer{
		nextConsumer:  consumertest.NewNop(),
		logger:        logger,
		consumerGroup: testClient,
	}

	err := c.Start(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, c.Shutdown(context.Background()))
	waitUntil(func() bool {
		return logObserver.FilterField(zap.Error(expectedErr)).Len() > 0
	}, 100, time.Millisecond*100)
	assert.True(t, logObserver.FilterField(zap.Error(expectedErr)).Len() > 0)
}

func TestTracesConsumerGroupHandler(t *testing.T) {
	views := MetricViews()
	require.NoError(t, view.Register(views...))
	defer view.Unregister(views...)

	c := tracesConsumerGroupHandler{
		unmarshaller: &otlpTracesPbUnmarshaller{},
		logger:       zap.NewNop(),
		ready:        make(chan bool),
		nextConsumer: consumertest.NewNop(),
	}

	testSession := testConsumerGroupSession{}
	err := c.Setup(testSession)
	require.NoError(t, err)
	_, ok := <-c.ready
	assert.False(t, ok)
	viewData, err := view.RetrieveData(statPartitionStart.Name())
	require.NoError(t, err)
	assert.Equal(t, 1, len(viewData))
	distData := viewData[0].Data.(*view.SumData)
	assert.Equal(t, float64(1), distData.Value)

	err = c.Cleanup(testSession)
	require.NoError(t, err)
	viewData, err = view.RetrieveData(statPartitionClose.Name())
	require.NoError(t, err)
	assert.Equal(t, 1, len(viewData))
	distData = viewData[0].Data.(*view.SumData)
	assert.Equal(t, float64(1), distData.Value)

	groupClaim := testConsumerGroupClaim{
		messageChan: make(chan *sarama.ConsumerMessage),
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		err = c.ConsumeClaim(testSession, groupClaim)
		require.NoError(t, err)
		wg.Done()
	}()

	groupClaim.messageChan <- &sarama.ConsumerMessage{}
	close(groupClaim.messageChan)
	wg.Wait()
}

func TestTracesConsumerGroupHandler_error_unmarshall(t *testing.T) {
	c := tracesConsumerGroupHandler{
		unmarshaller: &otlpTracesPbUnmarshaller{},
		logger:       zap.NewNop(),
		ready:        make(chan bool),
		nextConsumer: consumertest.NewNop(),
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	groupClaim := &testConsumerGroupClaim{
		messageChan: make(chan *sarama.ConsumerMessage),
	}
	go func() {
		err := c.ConsumeClaim(testConsumerGroupSession{}, groupClaim)
		require.Error(t, err)
		wg.Done()
	}()
	groupClaim.messageChan <- &sarama.ConsumerMessage{Value: []byte("!@#")}
	close(groupClaim.messageChan)
	wg.Wait()
}

func TestTracesConsumerGroupHandler_error_nextConsumer(t *testing.T) {
	consumerError := errors.New("failed to consumer")
	c := tracesConsumerGroupHandler{
		unmarshaller: &otlpTracesPbUnmarshaller{},
		logger:       zap.NewNop(),
		ready:        make(chan bool),
		nextConsumer: consumertest.NewErr(consumerError),
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	groupClaim := &testConsumerGroupClaim{
		messageChan: make(chan *sarama.ConsumerMessage),
	}
	go func() {
		e := c.ConsumeClaim(testConsumerGroupSession{}, groupClaim)
		assert.EqualError(t, e, consumerError.Error())
		wg.Done()
	}()

	td := pdata.NewTraces()
	td.ResourceSpans().AppendEmpty()
	bts, err := td.ToOtlpProtoBytes()
	require.NoError(t, err)
	groupClaim.messageChan <- &sarama.ConsumerMessage{Value: bts}
	close(groupClaim.messageChan)
	wg.Wait()
}

func TestNewLogsReceiver_version_err(t *testing.T) {
	c := Config{
		Encoding:        defaultEncoding,
		ProtocolVersion: "none",
	}
	r, err := newLogsReceiver(c, component.ReceiverCreateParams{}, defaultLogsUnmarshallers(), consumertest.NewNop())
	assert.Error(t, err)
	assert.Nil(t, r)
}

func TestNewLogsReceiver_encoding_err(t *testing.T) {
	c := Config{
		Encoding: "foo",
	}
	r, err := newLogsReceiver(c, component.ReceiverCreateParams{}, defaultLogsUnmarshallers(), consumertest.NewNop())
	require.Error(t, err)
	assert.Nil(t, r)
	assert.EqualError(t, err, errUnrecognizedEncoding.Error())
}

func TestNewLogsExporter_err_auth_type(t *testing.T) {
	c := Config{
		ProtocolVersion: "2.0.0",
		Authentication: kafkaexporter.Authentication{
			TLS: &configtls.TLSClientSetting{
				TLSSetting: configtls.TLSSetting{
					CAFile: "/doesnotexist",
				},
			},
		},
		Encoding: defaultEncoding,
		Metadata: kafkaexporter.Metadata{
			Full: false,
		},
	}
	r, err := newLogsReceiver(c, component.ReceiverCreateParams{}, defaultLogsUnmarshallers(), consumertest.NewNop())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to load TLS config")
	assert.Nil(t, r)
}

func TestLogsReceiverStart(t *testing.T) {
	testClient := testConsumerGroup{once: &sync.Once{}}
	c := kafkaLogsConsumer{
		nextConsumer:  consumertest.NewNop(),
		logger:        zap.NewNop(),
		consumerGroup: testClient,
	}

	err := c.Start(context.Background(), nil)
	require.NoError(t, err)
	c.Shutdown(context.Background())
}

func TestLogsReceiverStartConsume(t *testing.T) {
	testClient := testConsumerGroup{once: &sync.Once{}}
	c := kafkaLogsConsumer{
		nextConsumer:  consumertest.NewNop(),
		logger:        zap.NewNop(),
		consumerGroup: testClient,
	}
	ctx, cancelFunc := context.WithCancel(context.Background())
	c.cancelConsumeLoop = cancelFunc
	c.Shutdown(context.Background())
	err := c.consumeLoop(ctx, &logsConsumerGroupHandler{
		ready: make(chan bool),
	})
	assert.EqualError(t, err, context.Canceled.Error())
}

func TestLogsReceiver_error(t *testing.T) {
	zcore, logObserver := observer.New(zapcore.ErrorLevel)
	logger := zap.New(zcore)

	expectedErr := fmt.Errorf("handler error")
	testClient := testConsumerGroup{once: &sync.Once{}, err: expectedErr}
	c := kafkaLogsConsumer{
		nextConsumer:  consumertest.NewNop(),
		logger:        logger,
		consumerGroup: testClient,
	}

	err := c.Start(context.Background(), nil)
	require.NoError(t, err)
	c.Shutdown(context.Background())
	waitUntil(func() bool {
		return logObserver.FilterField(zap.Error(expectedErr)).Len() > 0
	}, 100, time.Millisecond*100)
	assert.True(t, logObserver.FilterField(zap.Error(expectedErr)).Len() > 0)
}

func TestLogsConsumerGroupHandler(t *testing.T) {
	views := MetricViews()
	view.Register(views...)
	defer view.Unregister(views...)

	c := logsConsumerGroupHandler{
		unmarshaller: &otlpLogsPbUnmarshaller{},
		logger:       zap.NewNop(),
		ready:        make(chan bool),
		nextConsumer: consumertest.NewNop(),
	}

	testSession := testConsumerGroupSession{}
	err := c.Setup(testSession)
	require.NoError(t, err)
	_, ok := <-c.ready
	assert.False(t, ok)
	viewData, err := view.RetrieveData(statPartitionStart.Name())
	require.NoError(t, err)
	assert.Equal(t, 1, len(viewData))
	distData := viewData[0].Data.(*view.SumData)
	assert.Equal(t, float64(1), distData.Value)

	err = c.Cleanup(testSession)
	require.NoError(t, err)
	viewData, err = view.RetrieveData(statPartitionClose.Name())
	require.NoError(t, err)
	assert.Equal(t, 1, len(viewData))
	distData = viewData[0].Data.(*view.SumData)
	assert.Equal(t, float64(1), distData.Value)

	groupClaim := testConsumerGroupClaim{
		messageChan: make(chan *sarama.ConsumerMessage),
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		err = c.ConsumeClaim(testSession, groupClaim)
		require.NoError(t, err)
		wg.Done()
	}()

	groupClaim.messageChan <- &sarama.ConsumerMessage{}
	close(groupClaim.messageChan)
	wg.Wait()
}

func TestLogsConsumerGroupHandler_error_unmarshall(t *testing.T) {
	c := logsConsumerGroupHandler{
		unmarshaller: &otlpLogsPbUnmarshaller{},
		logger:       zap.NewNop(),
		ready:        make(chan bool),
		nextConsumer: consumertest.NewNop(),
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	groupClaim := &testConsumerGroupClaim{
		messageChan: make(chan *sarama.ConsumerMessage),
	}
	go func() {
		err := c.ConsumeClaim(testConsumerGroupSession{}, groupClaim)
		require.Error(t, err)
		wg.Done()
	}()
	groupClaim.messageChan <- &sarama.ConsumerMessage{Value: []byte("!@#")}
	close(groupClaim.messageChan)
	wg.Wait()
}

func TestLogsConsumerGroupHandler_error_nextConsumer(t *testing.T) {
	consumerError := errors.New("failed to consumer")
	c := logsConsumerGroupHandler{
		unmarshaller: &otlpLogsPbUnmarshaller{},
		logger:       zap.NewNop(),
		ready:        make(chan bool),
		nextConsumer: consumertest.NewErr(consumerError),
	}

	wg := sync.WaitGroup{}
	wg.Add(1)
	groupClaim := &testConsumerGroupClaim{
		messageChan: make(chan *sarama.ConsumerMessage),
	}
	go func() {
		e := c.ConsumeClaim(testConsumerGroupSession{}, groupClaim)
		assert.EqualError(t, e, consumerError.Error())
		wg.Done()
	}()

	ld := testdata.GenerateLogDataOneLog()
	bts, err := ld.ToOtlpProtoBytes()
	require.NoError(t, err)
	groupClaim.messageChan <- &sarama.ConsumerMessage{Value: bts}
	close(groupClaim.messageChan)
	wg.Wait()
}

type testConsumerGroupClaim struct {
	messageChan chan *sarama.ConsumerMessage
}

var _ sarama.ConsumerGroupClaim = (*testConsumerGroupClaim)(nil)

const (
	testTopic               = "otlp_spans"
	testPartition           = 5
	testInitialOffset       = 6
	testHighWatermarkOffset = 4
)

func (t testConsumerGroupClaim) Topic() string {
	return testTopic
}

func (t testConsumerGroupClaim) Partition() int32 {
	return testPartition
}

func (t testConsumerGroupClaim) InitialOffset() int64 {
	return testInitialOffset
}

func (t testConsumerGroupClaim) HighWaterMarkOffset() int64 {
	return testHighWatermarkOffset
}

func (t testConsumerGroupClaim) Messages() <-chan *sarama.ConsumerMessage {
	return t.messageChan
}

type testConsumerGroupSession struct {
}

func (t testConsumerGroupSession) Commit() {
	panic("implement me")
}

var _ sarama.ConsumerGroupSession = (*testConsumerGroupSession)(nil)

func (t testConsumerGroupSession) Claims() map[string][]int32 {
	panic("implement me")
}

func (t testConsumerGroupSession) MemberID() string {
	panic("implement me")
}

func (t testConsumerGroupSession) GenerationID() int32 {
	panic("implement me")
}

func (t testConsumerGroupSession) MarkOffset(string, int32, int64, string) {
	panic("implement me")
}

func (t testConsumerGroupSession) ResetOffset(string, int32, int64, string) {
	panic("implement me")
}

func (t testConsumerGroupSession) MarkMessage(*sarama.ConsumerMessage, string) {
}

func (t testConsumerGroupSession) Context() context.Context {
	return context.Background()
}

type testConsumerGroup struct {
	once *sync.Once
	err  error
}

var _ sarama.ConsumerGroup = (*testConsumerGroup)(nil)

func (t testConsumerGroup) Consume(ctx context.Context, topics []string, handler sarama.ConsumerGroupHandler) error {
	t.once.Do(func() {
		t.err = handler.Setup(testConsumerGroupSession{})
	})
	return t.err
}

func (t testConsumerGroup) Errors() <-chan error {
	panic("implement me")
}

func (t testConsumerGroup) Close() error {
	return nil
}

func waitUntil(f func() bool, iterations int, sleepInterval time.Duration) {
	for i := 0; i < iterations; i++ {
		if f() {
			return
		}
		time.Sleep(sleepInterval)
	}
}
