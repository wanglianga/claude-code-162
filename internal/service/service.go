package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"recycle/internal/dispatch"
	"recycle/internal/models"
)

type Service struct {
	db   *pgxpool.Pool
	rdb  *redis.Client
	disp *dispatch.Dispatcher
}

func New(pool *pgxpool.Pool, rdb *redis.Client) *Service {
	return &Service{db: pool, rdb: rdb, disp: dispatch.New(rdb)}
}

func (s *Service) Dispatcher() *dispatch.Dispatcher { return s.disp }

// tx 运行事务并返回错误。
func (s *Service) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	t, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = t.Rollback(ctx) }()
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit(ctx)
}

// addEvent 在事务内写环节事件（时间线）。
func addEvent(ctx context.Context, t pgx.Tx, apptID int64, stage, eventType string,
	actorID *int64, actorRole string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	_, err := t.Exec(ctx, `
INSERT INTO stage_events(appointment_id, stage, event_type, actor_id, actor_role, payload)
VALUES($1,$2,$3,$4,$5,$6)`,
		apptID, stage, eventType, actorID, actorRole, mustJSON(payload))
	return err
}

// addOutbox 在同一事务内写发件箱，保证"业务变更 + 事件"原子性。
func addOutbox(ctx context.Context, t pgx.Tx, aggregateID, topic, eventType string, payload map[string]any) error {
	_, err := t.Exec(ctx, `
INSERT INTO outbox(aggregate_id, topic, event_type, payload)
VALUES($1,$2,$3,$4)`, aggregateID, topic, eventType, mustJSON(payload))
	return err
}

func mustJSON(v map[string]any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------- 查询辅助 ----------

func (s *Service) getAppointment(ctx context.Context, id int64) (*models.Appointment, error) {
	row := s.db.QueryRow(ctx, `
SELECT id, code, qr_token, resident_id, address, lng, lat, floor, has_elevator,
       window_start, window_end, promised_arrive_at, est_amount, est_weight_kg,
       price_detail, COALESCE(remark,''), stage, settle_method, created_at, updated_at
FROM appointments WHERE id=$1`, id)
	return scanAppointment(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAppointment(row rowScanner) (*models.Appointment, error) {
	var a models.Appointment
	var detail []byte
	var settleMethod *string
	if err := row.Scan(
		&a.ID, &a.Code, &a.QrToken, &a.ResidentID, &a.Address, &a.Lng, &a.Lat,
		&a.Floor, &a.HasElevator, &a.WindowStart, &a.WindowEnd, &a.PromisedArriveAt,
		&a.EstAmount, &a.EstWeightKg, &detail, &a.Remark, &a.Stage, &settleMethod,
		&a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		return nil, err
	}
	a.SettleMethod = settleMethod
	_ = json.Unmarshal(detail, &a.PriceDetail)
	return &a, nil
}

func apptRef(id int64) string { return fmt.Sprintf("appointment:%d", id) }
