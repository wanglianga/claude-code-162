package service

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"recycle/internal/bus"
	"recycle/internal/models"
	"recycle/internal/pricing"
)

type ItemInput struct {
	Category  string   `json:"category"`
	WeightKg  float64  `json:"weight_kg"`
	PhotoURLs []string `json:"photo_urls"`
}

type CreateAppointmentInput struct {
	Items       []ItemInput `json:"items"`
	Floor       int         `json:"floor"` // 可选，缺省取档案
	HasElevator *bool       `json:"has_elevator"`
	WindowStart time.Time   `json:"window_start"`
	WindowEnd   time.Time   `json:"window_end"`
	Remark      string      `json:"remark"`
}

// CreateAppointment 居民提交预约：校验品类 → 估价 → 落单 → 自动派单。
func (s *Service) CreateAppointment(ctx context.Context, residentID int64, in CreateAppointmentInput) (*models.Appointment, error) {
	if len(in.Items) == 0 {
		return nil, bizError("ITEMS_EMPTY", "至少填写一个回收品类")
	}
	if in.WindowEnd.Before(in.WindowStart) {
		return nil, bizError("WINDOW_INVALID", "上门时间窗结束时间早于开始时间")
	}

	// 居民档案（地址快照）
	var addr string
	var lng, lat float64
	var floor int
	var elevator bool
	err := s.db.QueryRow(ctx,
		`SELECT address,lng,lat,default_floor,default_elevator FROM residents WHERE account_id=$1`,
		residentID).Scan(&addr, &lng, &lat, &floor, &elevator)
	if err == pgx.ErrNoRows {
		return nil, bizError("NOT_RESIDENT", "当前账号不是居民")
	}
	if err != nil {
		return nil, err
	}
	if in.Floor > 0 {
		floor = in.Floor
	}
	if in.HasElevator != nil {
		elevator = *in.HasElevator
	}

	// 品类与当前市价
	prices := map[string]models.Category{}
	rows, err := s.db.Query(ctx,
		`SELECT code,name,unit_price,points_per_yuan,co2_per_kg,active,sort_no FROM categories WHERE active=true`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c models.Category
		if err := rows.Scan(&c.Code, &c.Name, &c.UnitPrice, &c.PointsPerYuan, &c.CO2PerKg, &c.Active, &c.SortNo); err != nil {
			rows.Close()
			return nil, err
		}
		prices[c.Code] = c
	}
	rows.Close()

	var lines []pricing.Line
	for _, it := range in.Items {
		c, ok := prices[it.Category]
		if !ok {
			return nil, bizError("CATEGORY_UNKNOWN", "未知或已停收品类: "+it.Category)
		}
		if it.WeightKg <= 0 {
			return nil, bizError("WEIGHT_INVALID", fmt.Sprintf("%s 预计重量必须大于 0", c.Name))
		}
		lines = append(lines, pricing.Line{Category: it.Category, WeightKg: it.WeightKg, Price: c.UnitPrice})
	}

	// 距离：取当前最近在岗回收员（派单预热），无则按站点距离
	nearestKm := s.nearestOnDutyDistanceKm(ctx, lng, lat)

	detail := pricing.Estimate(pricing.Input{
		Lines: lines, Floor: floor, HasElevator: elevator, DistanceKm: nearestKm,
	})

	code := "R" + time.Now().Format("20060102") + uuid.NewString()[:6]
	qrToken := uuid.NewString()

	var apptID int64
	err = s.tx(ctx, func(t pgx.Tx) error {
		err := t.QueryRow(ctx, `
INSERT INTO appointments(code,qr_token,resident_id,address,lng,lat,floor,has_elevator,
       window_start,window_end,est_amount,est_weight_kg,price_detail,remark)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			code, qrToken, residentID, addr, lng, lat, floor, elevator,
			in.WindowStart, in.WindowEnd, detail.EstAmount, detail.TotalWeightKg,
			mustJSON(detailMap(detail)), in.Remark).Scan(&apptID)
		if err != nil {
			return err
		}
		for i, it := range in.Items {
			c := prices[it.Category]
			photos := it.PhotoURLs
			if photos == nil {
				photos = []string{}
			}
			if _, err := t.Exec(ctx, `
INSERT INTO appointment_items(appointment_id,category,est_weight_kg,est_unit_price,est_amount,photo_urls)
VALUES($1,$2,$3,$4,$5,$6)`,
				apptID, it.Category, it.WeightKg, c.UnitPrice, detail.Lines[i].Amount, photos); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, t, apptID, models.StageAppointment, "appointment.created",
			&residentID, "resident", map[string]any{
				"code": code, "items": len(in.Items), "est_amount": detail.EstAmount,
				"est_weight_kg": detail.TotalWeightKg, "window_start": in.WindowStart,
				"window_end": in.WindowEnd, "floor": floor, "has_elevator": elevator,
				"price_detail": detail,
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicAppointment, bus.EventTypeAppointmentCreated, map[string]any{
			"appointment_id": apptID, "code": code, "resident_id": residentID,
			"est_amount": detail.EstAmount, "est_weight_kg": detail.TotalWeightKg,
			"lng": lng, "lat": lat, "window_start": in.WindowStart, "window_end": in.WindowEnd,
		})
	})
	if err != nil {
		return nil, err
	}

	// 事务后自动派单（派单失败不影响预约，可重试/手工改派）
	if err := s.autoOffer(ctx, apptID); err != nil {
		// 仅记录为系统事件
		_ = s.recordSystemNote(ctx, apptID, "dispatch.auto_offer_failed", map[string]any{"error": err.Error()})
	}
	return s.GetAppointment(ctx, apptID)
}

func detailMap(d pricing.Detail) map[string]any {
	return map[string]any{
		"formula":         d.Formula,
		"base_amount":     d.BaseAmount,
		"floor_surcharge": d.FloorSurcharge,
		"floor_reason":    d.FloorReason,
		"distance_adjust": d.DistanceAdjust,
		"distance_reason": d.DistanceReason,
		"total_weight_kg": d.TotalWeightKg,
		"est_amount":      d.EstAmount,
		"est_points":      d.EstPoints,
		"lines":           d.Lines,
	}
}

func (s *Service) nearestOnDutyDistanceKm(ctx context.Context, lng, lat float64) float64 {
	onDuty, err := s.onDutyRecyclers(ctx, time.Now())
	if err != nil || len(onDuty) == 0 {
		return 0
	}
	cands, err := s.disp.Nearby(ctx, lng, lat, 50, onDuty)
	if err != nil || len(cands) == 0 {
		// Redis 未就绪时回退到 DB 坐标
		return s.nearestFromDB(ctx, lng, lat, onDuty)
	}
	return cands[0].DistanceKm
}

func (s *Service) nearestFromDB(ctx context.Context, lng, lat float64, onDuty map[int64]bool) float64 {
	rows, err := s.db.Query(ctx,
		`SELECT account_id,lng,lat FROM recyclers WHERE duty_status='on'`)
	if err != nil {
		return 0
	}
	defer rows.Close()
	best := 9999.0
	for rows.Next() {
		var id int64
		var rl, ra float64
		if rows.Scan(&id, &rl, &ra) != nil {
			continue
		}
		if !onDuty[id] {
			continue
		}
		if d := pricing.HaversineKm(lng, lat, rl, ra); d < best {
			best = d
		}
	}
	if best > 9000 {
		return 0
	}
	return best
}

// onDutyRecyclers 返回在指定时刻排班覆盖且在岗的回收员集合。
func (s *Service) onDutyRecyclers(ctx context.Context, at time.Time) (map[int64]bool, error) {
	minute := at.Hour()*60 + at.Minute()
	rows, err := s.db.Query(ctx, `
SELECT rs.recycler_id
FROM recycler_schedules rs
JOIN recyclers r ON r.account_id = rs.recycler_id
WHERE rs.work_date = $1 AND rs.active = true
  AND rs.start_min <= $2 AND rs.end_min >= $2
  AND r.duty_status = 'on'`, at.Format("2006-01-02"), minute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, nil
}

// GetAppointment 查询预约及品类明细。
func (s *Service) GetAppointment(ctx context.Context, id int64) (*models.Appointment, error) {
	a, err := s.getAppointment(ctx, id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT id,category,est_weight_kg,est_unit_price,est_amount,photo_urls,added_later,added_by
FROM appointment_items WHERE appointment_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var it models.AppointmentItem
		var photos []string
		if err := rows.Scan(&it.ID, &it.Category, &it.EstWeightKg, &it.EstUnitPrice,
			&it.EstAmount, &photos, &it.AddedLater, &it.AddedBy); err != nil {
			return nil, err
		}
		it.PhotoURLs = photos
		a.Items = append(a.Items, it)
	}
	return a, nil
}

// ListAppointments 按居民/状态查询。
func (s *Service) ListAppointments(ctx context.Context, residentID int64, stage string, limit int) ([]models.Appointment, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT id, code, qr_token, resident_id, address, lng, lat, floor, has_elevator,
       window_start, window_end, promised_arrive_at, est_amount, est_weight_kg,
       price_detail, COALESCE(remark,''), stage, settle_method, created_at, updated_at
FROM appointments WHERE resident_id=$1`
	args := []any{residentID}
	if stage != "" {
		q += " AND stage=$2"
		args = append(args, stage)
		q += " ORDER BY id DESC LIMIT $3"
	} else {
		q += " ORDER BY id DESC LIMIT $2"
	}
	args = append(args, limit)
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Appointment
	for rows.Next() {
		a, err := scanAppointment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, nil
}

// CancelAppointment 居民取消（未现场定价前允许）。
func (s *Service) CancelAppointment(ctx context.Context, residentID, apptID int64) error {
	a, err := s.GetAppointment(ctx, apptID)
	if err != nil {
		return err
	}
	if a.ResidentID != residentID {
		return ErrForbidden
	}
	if a.Stage != "booked" && a.Stage != "assigned" {
		return bizError("STAGE_CONFLICT", "当前环节不可取消（已上门/定价）")
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `UPDATE appointments SET stage='canceled',updated_at=now() WHERE id=$1`, apptID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`UPDATE dispatches SET status='canceled' WHERE appointment_id=$1 AND status IN ('offered','accepted')`,
			apptID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageAppointment, "appointment.canceled",
			&residentID, "resident", nil); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicAppointment, "AppointmentCanceled", map[string]any{
			"appointment_id": apptID,
		})
	})
}

// AddItem 居民上门时临时加品类（现场定价之前）。
func (s *Service) AddItem(ctx context.Context, residentID, apptID int64, in ItemInput) (*models.AppointmentItem, error) {
	a, err := s.GetAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	if a.ResidentID != residentID {
		return nil, ErrForbidden
	}
	if a.Stage == "canceled" || a.Stage == "priced" || a.Stage == "settled" ||
		a.Stage == "inbound" || a.Stage == "certified" || a.Stage == "closed" {
		return nil, bizError("STAGE_CONFLICT", "现场定价/结算后不能再加品类，如未称重请联系回收员")
	}
	var c models.Category
	err = s.db.QueryRow(ctx,
		`SELECT code,name,unit_price,points_per_yuan,co2_per_kg,active,sort_no FROM categories WHERE code=$1 AND active=true`,
		in.Category).Scan(&c.Code, &c.Name, &c.UnitPrice, &c.PointsPerYuan, &c.CO2PerKg, &c.Active, &c.SortNo)
	if err == pgx.ErrNoRows {
		return nil, bizError("CATEGORY_UNKNOWN", "未知或已停收品类: "+in.Category)
	}
	if err != nil {
		return nil, err
	}
	if in.WeightKg <= 0 {
		return nil, bizError("WEIGHT_INVALID", "预计重量必须大于 0")
	}
	lineAmt := math.Round(pricing.SettleAmount(in.WeightKg, c.UnitPrice)*0.95*100) / 100
	var itemID int64
	photos := in.PhotoURLs
	if photos == nil {
		photos = []string{}
	}
	err = s.tx(ctx, func(t pgx.Tx) error {
		if err := t.QueryRow(ctx, `
INSERT INTO appointment_items(appointment_id,category,est_weight_kg,est_unit_price,est_amount,photo_urls,added_later,added_by)
VALUES($1,$2,$3,$4,$5,$6,true,$7) RETURNING id`,
			apptID, in.Category, in.WeightKg, c.UnitPrice, lineAmt, photos, residentID).Scan(&itemID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET est_weight_kg = est_weight_kg + $2, est_amount = est_amount + $3, updated_at=now() WHERE id=$1`,
			apptID, in.WeightKg, lineAmt); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageAppointment, "appointment.item_added",
			&residentID, "resident", map[string]any{
				"category": in.Category, "est_weight_kg": in.WeightKg, "est_amount": lineAmt,
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicAppointment, bus.EventTypeAppointmentAdded, map[string]any{
			"appointment_id": apptID, "category": in.Category, "weight_kg": in.WeightKg,
		})
	})
	if err != nil {
		return nil, err
	}
	return &models.AppointmentItem{
		ID: itemID, Category: in.Category, EstWeightKg: in.WeightKg,
		EstUnitPrice: c.UnitPrice, EstAmount: lineAmt, PhotoURLs: in.PhotoURLs,
		AddedLater: true, AddedBy: &residentID,
	}, nil
}

func (s *Service) recordSystemNote(ctx context.Context, apptID int64, eventType string, payload map[string]any) error {
	return s.tx(ctx, func(t pgx.Tx) error {
		return addEvent(ctx, t, apptID, "system", eventType, nil, "system", payload)
	})
}
