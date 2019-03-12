// Copyright (c) 2018 Ashley Jeffs
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package reader

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Jeffail/benthos/lib/log"
	"github.com/Jeffail/benthos/lib/message"
	"github.com/Jeffail/benthos/lib/metrics"
	"github.com/Jeffail/benthos/lib/types"
	btls "github.com/Jeffail/benthos/lib/util/tls"
	"github.com/Shopify/sarama"
)

//------------------------------------------------------------------------------

// KafkaBalancedConfig contains configuration for the KafkaBalanced input type.
type KafkaBalancedConfig struct {
	Addresses       []string    `json:"addresses" yaml:"addresses"`
	ClientID        string      `json:"client_id" yaml:"client_id"`
	ConsumerGroup   string      `json:"consumer_group" yaml:"consumer_group"`
	CommitPeriod    string      `json:"commit_period" yaml:"commit_period"`
	Topics          []string    `json:"topics" yaml:"topics"`
	StartFromOldest bool        `json:"start_from_oldest" yaml:"start_from_oldest"`
	TargetVersion   string      `json:"target_version" yaml:"target_version"`
	MaxBatchCount   int         `json:"max_batch_count" yaml:"max_batch_count"`
	TLS             btls.Config `json:"tls" yaml:"tls"`
}

// NewKafkaBalancedConfig creates a new KafkaBalancedConfig with default values.
func NewKafkaBalancedConfig() KafkaBalancedConfig {
	return KafkaBalancedConfig{
		Addresses:       []string{"localhost:9092"},
		ClientID:        "benthos_kafka_input",
		ConsumerGroup:   "benthos_consumer_group",
		CommitPeriod:    "1s",
		Topics:          []string{"benthos_stream"},
		StartFromOldest: true,
		TargetVersion:   sarama.V1_0_0_0.String(),
		MaxBatchCount:   1,
		TLS:             btls.NewConfig(),
	}
}

//------------------------------------------------------------------------------

// KafkaBalanced is an input type that reads from a Kafka cluster by balancing
// partitions across other consumers of the same consumer group.
type KafkaBalanced struct {
	version      sarama.KafkaVersion
	tlsConf      *tls.Config
	addresses    []string
	topics       []string
	commitPeriod time.Duration

	cMut    sync.Mutex
	group   sarama.ConsumerGroup
	session sarama.ConsumerGroupSession
	msgChan chan *sarama.ConsumerMessage

	offsets map[string]map[int32]int64

	mRebalanced metrics.StatCounter

	conf  KafkaBalancedConfig
	stats metrics.Type
	log   log.Modular
}

// NewKafkaBalanced creates a new KafkaBalanced input type.
func NewKafkaBalanced(
	conf KafkaBalancedConfig, log log.Modular, stats metrics.Type,
) (*KafkaBalanced, error) {
	k := KafkaBalanced{
		conf:        conf,
		stats:       stats,
		log:         log,
		offsets:     map[string]map[int32]int64{},
		mRebalanced: stats.GetCounter("rebalanced"),
	}
	if conf.MaxBatchCount < 1 {
		return nil, errors.New("max_batch_count must be greater than or equal to 1")
	}
	if conf.TLS.Enabled {
		var err error
		if k.tlsConf, err = conf.TLS.Get(); err != nil {
			return nil, err
		}
	}
	for _, addr := range conf.Addresses {
		for _, splitAddr := range strings.Split(addr, ",") {
			if len(splitAddr) > 0 {
				k.addresses = append(k.addresses, splitAddr)
			}
		}
	}
	for _, t := range conf.Topics {
		for _, splitTopics := range strings.Split(t, ",") {
			if len(splitTopics) > 0 {
				k.topics = append(k.topics, splitTopics)
			}
		}
	}
	if tout := conf.CommitPeriod; len(tout) > 0 {
		var err error
		if k.commitPeriod, err = time.ParseDuration(tout); err != nil {
			return nil, fmt.Errorf("failed to parse commit period string: %v", err)
		}
	}
	var err error
	if k.version, err = sarama.ParseKafkaVersion(conf.TargetVersion); err != nil {
		return nil, err
	}
	return &k, nil
}

//------------------------------------------------------------------------------

// Setup is run at the beginning of a new session, before ConsumeClaim.
func (k *KafkaBalanced) Setup(sesh sarama.ConsumerGroupSession) error {
	k.cMut.Lock()
	k.session = sesh
	k.cMut.Unlock()
	k.mRebalanced.Incr(1)
	return nil
}

// Cleanup is run at the end of a session, once all ConsumeClaim goroutines have
// exited but before the offsets are committed for the very last time.
func (k *KafkaBalanced) Cleanup(sesh sarama.ConsumerGroupSession) error {
	k.cMut.Lock()
	k.session = nil
	if k.msgChan != nil {
		close(k.msgChan)
		k.msgChan = nil
	}
	k.cMut.Unlock()
	return nil
}

// ConsumeClaim must start a consumer loop of ConsumerGroupClaim's Messages().
// Once the Messages() channel is closed, the Handler must finish its processing
// loop and exit.
func (k *KafkaBalanced) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		k.msgChan <- msg
	}
	return nil
}

//------------------------------------------------------------------------------

func (k *KafkaBalanced) setOffset(topic string, partition int32, offset int64) {
	var topicMap map[int32]int64
	var exists bool
	if topicMap, exists = k.offsets[topic]; !exists {
		topicMap = map[int32]int64{}
		k.offsets[topic] = topicMap
	}
	topicMap[partition] = offset
}

func (k *KafkaBalanced) closeGroup() {
	k.cMut.Lock()
	group := k.group
	k.group = nil
	k.cMut.Unlock()

	if group != nil {
		group.Close()
	}
}

//------------------------------------------------------------------------------

// Connect establishes a KafkaBalanced connection.
func (k *KafkaBalanced) Connect() error {
	k.cMut.Lock()
	defer k.cMut.Unlock()
	if k.msgChan != nil {
		return nil
	}

	config := sarama.NewConfig()
	config.ClientID = k.conf.ClientID
	config.Net.DialTimeout = time.Second
	config.Version = k.version
	config.Consumer.Return.Errors = true
	config.Consumer.Offsets.CommitInterval = k.commitPeriod
	config.Net.TLS.Enable = k.conf.TLS.Enabled
	if k.conf.TLS.Enabled {
		config.Net.TLS.Config = k.tlsConf
	}
	if k.conf.StartFromOldest {
		config.Consumer.Offsets.Initial = sarama.OffsetOldest
	}

	// Start a new consumer group
	group, err := sarama.NewConsumerGroup(k.addresses, k.conf.ConsumerGroup, config)
	if err != nil {
		return err
	}

	// Handle errors
	go func() {
		for {
			gerr, open := <-group.Errors()
			if !open {
				return
			}
			if gerr != nil {
				k.log.Errorf("KafkaBalanced message recv error: %v\n", gerr)
			}
		}
	}()

	// Handle session
	go func() {
		ctx := context.Background()
		gerr := group.Consume(ctx, k.topics, k)
		if gerr != nil {
			k.log.Errorf("KafkaBalanced group session error: %v\n", gerr)
			k.Cleanup(nil)
		}
		group.Close()
	}()

	k.msgChan = make(chan *sarama.ConsumerMessage, k.conf.MaxBatchCount)
	k.group = group
	k.offsets = map[string]map[int32]int64{}

	k.log.Infof("Receiving KafkaBalanced messages from addresses: %s\n", k.addresses)
	return nil
}

// Read attempts to read a message from a KafkaBalanced topic.
func (k *KafkaBalanced) Read() (types.Message, error) {
	k.cMut.Lock()
	msgChan := k.msgChan
	k.cMut.Unlock()

	if msgChan == nil {
		return nil, types.ErrNotConnected
	}

	msg := message.New(nil)
	addPart := func(data *sarama.ConsumerMessage) {
		part := message.NewPart(data.Value)

		meta := part.Metadata()
		for _, hdr := range data.Headers {
			meta.Set(string(hdr.Key), string(hdr.Value))
		}
		meta.Set("kafka_key", string(data.Key))
		meta.Set("kafka_partition", strconv.Itoa(int(data.Partition)))
		meta.Set("kafka_topic", data.Topic)
		meta.Set("kafka_offset", strconv.Itoa(int(data.Offset)))
		meta.Set("kafka_timestamp_unix", strconv.FormatInt(data.Timestamp.Unix(), 10))

		msg.Append(part)

		k.setOffset(data.Topic, data.Partition, data.Offset)
	}

	data, open := <-msgChan
	if !open {
		return nil, types.ErrNotConnected
	}
	addPart(data)

batchLoop:
	for i := 1; i < k.conf.MaxBatchCount; i++ {
		select {
		case data, open = <-msgChan:
			if !open {
				return nil, types.ErrNotConnected
			}
			addPart(data)
		default:
			// Drained the buffer
			break batchLoop
		}
	}

	if msg.Len() == 0 {
		return nil, types.ErrTimeout
	}
	return msg, nil
}

// Acknowledge instructs whether the current offset should be committed.
func (k *KafkaBalanced) Acknowledge(err error) error {
	if err == nil {
		k.cMut.Lock()
		if k.session != nil {
			for topic, v := range k.offsets {
				for part, offset := range v {
					k.session.MarkOffset(topic, part, offset+1, "")
				}
			}
		}
		k.cMut.Unlock()
	}
	return nil
}

// CloseAsync shuts down the KafkaBalanced input and stops processing requests.
func (k *KafkaBalanced) CloseAsync() {
	go k.closeGroup()
}

// WaitForClose blocks until the KafkaBalanced input has closed down.
func (k *KafkaBalanced) WaitForClose(timeout time.Duration) error {
	return nil
}

//------------------------------------------------------------------------------
