package events

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"time"

	"github.com/IBM/sarama"

	"github.com/jackc/pgx/v5/pgxpool"
	"recycle/internal/bus"
)

// EnsureTopics 创建主题（本地单 broker，1 副本）。
func EnsureTopics(brokers []string) error {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	cfg.Net.MaxOpenRequests = 1
	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		return err
	}
	defer admin.Close()
	for _, t := range bus.AllTopics() {
		err := admin.CreateTopic(t, &sarama.TopicDetail{
			NumPartitions:     3,
			ReplicationFactor: 1,
		}, false)
		if err != nil {
			// 已存在忽略
			if ke, ok := err.(*sarama.TopicError); ok && ke.Err == sarama.ErrTopicAlreadyExists {
				continue
			}
			// 兼容错误文本
			if contains(err.Error(), "already exists") {
				continue
			}
			log.Printf("create topic %s: %v", t, err)
		}
	}
	return nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// OutboxRelay 轮询 outbox 未发布记录投递 Kafka，成功后标记 published。
// 至少一次投递：消费者必须幂等。
type OutboxRelay struct {
	pool     *pgxpool.Pool
	producer sarama.SyncProducer
}

func NewOutboxRelay(pool *pgxpool.Pool, brokers []string) (*OutboxRelay, error) {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	cfg.Producer.RequiredAcks = sarama.WaitForAll
	cfg.Producer.Return.Successes = true
	cfg.Producer.Idempotent = true
	cfg.Net.MaxOpenRequests = 1
	p, err := sarama.NewSyncProducer(brokers, cfg)
	if err != nil {
		return nil, err
	}
	return &OutboxRelay{pool: pool, producer: p}, nil
}

func (r *OutboxRelay) Close() error { return r.producer.Close() }

type outboxRow struct {
	ID      int64
	Topic   string
	Type    string
	Payload []byte
	AggID   string
}

func (r *OutboxRelay) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.pump(ctx); err != nil {
				log.Printf("outbox relay: %v", err)
			}
		}
	}
}

func (r *OutboxRelay) pump(ctx context.Context) error {
	rows, err := r.pool.Query(ctx, `
SELECT id, topic, event_type, payload, aggregate_id
FROM outbox WHERE published=false ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED`)
	if err != nil {
		return err
	}
	var batch []outboxRow
	for rows.Next() {
		var o outboxRow
		if err := rows.Scan(&o.ID, &o.Topic, &o.Type, &o.Payload, &o.AggID); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, o)
	}
	rows.Close()

	for _, o := range batch {
		// 信封：事件类型放 header，payload 为 body
		msg := &sarama.ProducerMessage{
			Topic: o.Topic,
			Key:   sarama.StringEncoder(o.AggID), // 同一预约有序
			Headers: []sarama.RecordHeader{
				{Key: []byte("event_type"), Value: []byte(o.Type)},
				{Key: []byte("event_id"), Value: []byte(strconv.FormatInt(o.ID, 10))},
			},
			Value: sarama.ByteEncoder(o.Payload),
		}
		if _, _, err := r.producer.SendMessage(msg); err != nil {
			return err
		}
		if _, err := r.pool.Exec(ctx,
			`UPDATE outbox SET published=true, published_at=now() WHERE id=$1`, o.ID); err != nil {
			return err
		}
	}
	return nil
}

// DecodePayload 解析事件体。
func DecodePayload(msg *sarama.ConsumerMessage) (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal(msg.Value, &m)
	return m, err
}
