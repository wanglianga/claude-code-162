package events

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"

	"github.com/IBM/sarama"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"recycle/internal/bus"
	"recycle/internal/service"
)

// Worker 消费业务事件，执行：
//   - certificate handler：入站复称后自动开具环保凭证；
//   - notification handler：积分到账/支付失败/复称差异等站内通知；
//   - analytics handler：街道资源流向日统计。
//
// 每条消息的副作用与 worker_checkpoints 插入在同一事务提交，
// 配合 Kafka 至少一次投递，实现幂等的最终一致。
type Worker struct {
	pool   *pgxpool.Pool
	svc    *service.Service
	group  string
	topics []string
}

func NewWorker(pool *pgxpool.Pool, svc *service.Service) *Worker {
	return &Worker{
		pool:   pool,
		svc:    svc,
		group:  "recycle-service-workers",
		topics: bus.AllTopics(),
	}
}

func (w *Worker) Run(ctx context.Context, brokers []string) error {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	cfg.Consumer.Group.Rebalance.GroupStrategies = []sarama.BalanceStrategy{sarama.NewBalanceStrategyRoundRobin()}
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	client, err := sarama.NewConsumerGroup(brokers, w.group, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	for {
		err := client.Consume(ctx, w.topics, w)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("consumer group: %v", err)
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (w *Worker) Setup(sarama.ConsumerGroupSession) error   { return nil }
func (w *Worker) Cleanup(sarama.ConsumerGroupSession) error { return nil }

func (w *Worker) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	ctx := sess.Context()
	for msg := range claim.Messages() {
		if err := w.handle(ctx, msg); err != nil {
			log.Printf("handle event %s/%d: %v", msg.Topic, msg.Offset, err)
			continue // 不提交，等待重试
		}
		sess.MarkMessage(msg, "")
	}
	return nil
}

func headerOf(msg *sarama.ConsumerMessage, key string) string {
	for _, h := range msg.Headers {
		if string(h.Key) == key {
			return string(h.Value)
		}
	}
	return ""
}

func (w *Worker) handle(ctx context.Context, msg *sarama.ConsumerMessage) error {
	eventType := headerOf(msg, "event_type")
	eventID, _ := strconv.ParseInt(headerOf(msg, "event_id"), 10, 64)
	var payload map[string]any
	if len(msg.Value) > 0 {
		if err := json.Unmarshal(msg.Value, &payload); err != nil {
			return err
		}
	}
	apptID := int64From(payload["appointment_id"])

	// 数据库被重置后 Kafka 可能残留历史消息：引用的预约已不存在时直接跳过位点，
	// 避免毒消息无限重试阻塞分区。
	if apptID > 0 {
		var exists bool
		if err := w.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM appointments WHERE id=$1)`, apptID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			log.Printf("skip stale event %s for missing appointment %d", eventType, apptID)
			return nil
		}
	}

	// 1) 凭证自动签发：IssueCertificate 自身幂等（已有凭证直接返回），
	//    再登记 checkpoint；任何一步失败都不提交位点，重放安全。
	if eventType == bus.EventTypeInboundReweighed && apptID > 0 {
		if _, err := w.svc.IssueCertificate(ctx, apptID); err != nil {
			return err
		}
		if err := w.markDone(ctx, "certificate", eventID, msg.Topic); err != nil {
			return err
		}
	}

	// 2) 通知
	if apptID > 0 {
		if err := w.notification(ctx, eventID, msg.Topic, eventType, apptID, payload); err != nil {
			return err
		}
	}

	// 3) 街道统计：结算成功后按现场品类累计参与/重量/金额/积分
	if eventType == bus.EventTypeSettleSucceeded && apptID > 0 {
		if err := w.analytics(ctx, eventID, msg.Topic, apptID, payload); err != nil {
			return err
		}
	}
	return nil
}

// once 已处理则跳过。
func (w *Worker) once(ctx context.Context, handler string, eventID int64, fn func(pgx.Tx) error) error {
	if eventID == 0 {
		return nil
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM worker_checkpoints WHERE event_id=$1 AND handler=$2)`,
		eventID, handler).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Rollback(ctx)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_checkpoints(event_id,handler,topic) VALUES($1,$2,$3)`,
		eventID, handler, "certificate"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (w *Worker) markDone(ctx context.Context, handler string, eventID int64, topic string) error {
	if eventID == 0 {
		return nil
	}
	_, err := w.pool.Exec(ctx,
		`INSERT INTO worker_checkpoints(event_id,handler,topic) VALUES($1,$2,$3)
		 ON CONFLICT DO NOTHING`, eventID, handler, topic)
	return err
}

// notification 关键事件给居民发站内通知（幂等）。
func (w *Worker) notification(ctx context.Context, eventID int64, topic, eventType string, apptID int64, payload map[string]any) error {
	handlers := map[string]struct {
		title   string
		content string
		who     string // resident / recycler
	}{
		bus.EventTypeSettleSucceeded:   {"结算完成", "您的回收款已结算：现金兑付/积分已到账", "resident"},
		bus.EventTypeSettleFailed:      {"结算失败", "本次结算未成功，请更换支付方式后重试", "resident"},
		bus.EventTypeInboundReweighed:  {"站点复称完成", "回收物已入站复称，环保凭证即将开具", "resident"},
		bus.EventTypeCertificateIssued: {"环保凭证已开具", "您本次回收的环保凭证已生成，可在预约详情查看", "resident"},
		bus.EventTypeDispatchAccepted:  {"回收员已接单", "回收员已接单，请保持电话畅通", "resident"},
	}
	h, ok := handlers[eventType]
	if !ok {
		return nil
	}
	var residentID int64
	if h.who == "resident" {
		residentID = int64From(payload["resident_id"])
	}
	if residentID == 0 {
		if err := w.pool.QueryRow(ctx,
			`SELECT resident_id FROM appointments WHERE id=$1`, apptID).Scan(&residentID); err != nil {
			return err
		}
	}
	// 预约或居民已不存在（例如重置数据库后 Kafka 残留的历史消息）：
	// 登记 checkpoint 跳过，避免毒消息无限阻塞该分区。
	var apptExists, acctExists bool
	if err := w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM appointments WHERE id=$1)`, apptID).Scan(&apptExists); err != nil {
		return err
	}
	if err := w.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=$1)`, residentID).Scan(&acctExists); err != nil {
		return err
	}
	if !apptExists || !acctExists {
		return w.markDone(ctx, "notification", eventID, topic)
	}
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM worker_checkpoints WHERE event_id=$1 AND handler='notification')`,
		eventID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Rollback(ctx)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO notifications(account_id,appointment_id,title,content)
VALUES($1,$2,$3,$4)`, residentID, apptID, h.title, h.content); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_checkpoints(event_id,handler,topic) VALUES($1,'notification',$2)
		 ON CONFLICT DO NOTHING`, eventID, topic); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// analytics 结算成功 -> 街道日统计（按现场称重品类）。
func (w *Worker) analytics(ctx context.Context, eventID int64, topic string, apptID int64, payload map[string]any) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM worker_checkpoints WHERE event_id=$1 AND handler='analytics')`,
		eventID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Rollback(ctx)
	}
	rows, err := tx.Query(ctx, `
SELECT wi.category, wi.weight_kg - wi.rejected_kg, wi.amount
FROM weigh_items wi
JOIN weigh_records wr ON wr.id = wi.weigh_record_id
WHERE wr.appointment_id=$1 AND wr.phase='onsite'
  AND wr.status IN ('confirmed','adjusted')
  AND wr.seq = (SELECT max(seq) FROM weigh_records WHERE appointment_id=$1 AND phase='onsite')`,
		apptID)
	if err != nil {
		return err
	}
	type statLine struct {
		category string
		weight   float64
		amount   float64
	}
	var lines []statLine
	for rows.Next() {
		var l statLine
		if err := rows.Scan(&l.category, &l.weight, &l.amount); err != nil {
			rows.Close()
			return err
		}
		lines = append(lines, l)
	}
	rows.Close()

	var residentID int64
	if err := tx.QueryRow(ctx, `SELECT resident_id FROM appointments WHERE id=$1`, apptID).Scan(&residentID); err != nil {
		return err
	}
	points := int64From(payload["points"])
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `
INSERT INTO street_daily_stats(stat_date,category,appointment_cnt,weight_kg,amount,points,participants)
VALUES(CURRENT_DATE,$1,1,$2,$3,$4,1)
ON CONFLICT (stat_date,category) DO UPDATE SET
  appointment_cnt = street_daily_stats.appointment_cnt + 1,
  weight_kg = street_daily_stats.weight_kg + EXCLUDED.weight_kg,
  amount = street_daily_stats.amount + EXCLUDED.amount,
  points = street_daily_stats.points + EXCLUDED.points,
  participants = street_daily_stats.participants + 1`,
			l.category, l.weight, l.amount, points); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO worker_checkpoints(event_id,handler,topic) VALUES($1,'analytics',$2)
		 ON CONFLICT DO NOTHING`, eventID, topic); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func int64From(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	}
	return 0
}
