package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"recycle/internal/bus"
	"recycle/internal/dispatch"
	"recycle/internal/models"
	"recycle/internal/pricing"
)

// autoOffer 自动派单：取排班内 + 在岗回收员，Redis GEO 按距离排序，
// 写入 offered 派单（推送给最近回收员）。距离回退到 DB 坐标计算。
func (s *Service) autoOffer(ctx context.Context, apptID int64) error {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return err
	}
	if a.Stage != "booked" {
		return nil
	}
	onDuty, err := s.onDutyRecyclers(ctx, time.Now())
	if err != nil {
		return err
	}
	cands, err := s.disp.Nearby(ctx, a.Lng, a.Lat, 50, onDuty)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		// 回退：DB 坐标
		rows, qerr := s.db.Query(ctx,
			`SELECT account_id,lng,lat FROM recyclers WHERE duty_status='on'`)
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var id int64
			var rl, ra float64
			if rows.Scan(&id, &rl, &ra) != nil || !onDuty[id] {
				continue
			}
			cands = append(cands, dispatch.Candidate{RecyclerID: id, DistanceKm: pricing.HaversineKm(a.Lng, a.Lat, rl, ra)})
		}
		rows.Close()
		// 按距离升序
		for i := 0; i < len(cands); i++ {
			for j := i + 1; j < len(cands); j++ {
				if cands[j].DistanceKm < cands[i].DistanceKm {
					cands[i], cands[j] = cands[j], cands[i]
				}
			}
		}
	}
	if len(cands) == 0 {
		return s.recordSystemNote(ctx, apptID, "dispatch.no_available_recycler", map[string]any{
			"hint": "当前时间窗内无在岗回收员，等待运营改派",
		})
	}

	// 只推最近 3 名；首名为默认接单候选
	best := cands[0]
	if len(cands) > 3 {
		cands = cands[:3]
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		for _, c := range cands {
			var stationID int64
			if err := t.QueryRow(ctx, `SELECT station_id FROM recyclers WHERE account_id=$1`, c.RecyclerID).Scan(&stationID); err != nil {
				return err
			}
			if _, err := t.Exec(ctx, `
INSERT INTO dispatches(appointment_id,recycler_id,station_id,distance_km,status,assigned_by)
VALUES($1,$2,$3,$4,'offered','auto')
ON CONFLICT (appointment_id,recycler_id)
DO UPDATE SET status='offered', offered_at=now(), distance_km=EXCLUDED.distance_km`,
				apptID, c.RecyclerID, stationID, c.DistanceKm); err != nil {
				return err
			}
		}
		// 承诺到达：30 分钟 + 距离×2 分钟
		eta := time.Now().Add(time.Duration(30+int(best.DistanceKm*2)) * time.Minute)
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET promised_arrive_at=$2, updated_at=now() WHERE id=$1 AND promised_arrive_at IS NULL`,
			apptID, eta); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, "dispatch", "dispatch.offered", nil, "system", map[string]any{
			"candidate_count":    len(cands),
			"candidates":         cands,
			"nearest_id":         best.RecyclerID,
			"nearest_km":         best.DistanceKm,
			"promised_arrive_at": eta,
		}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicDispatch, bus.EventTypeDispatchOffered, map[string]any{
			"appointment_id": apptID, "candidates": cands, "nearest_id": best.RecyclerID,
		})
	})
}

// AutoOfferHTTP 暴露给运营：重新自动派单（之前无人接单时）。
func (s *Service) AutoOffer(ctx context.Context, apptID int64) error {
	return s.autoOffer(ctx, apptID)
}

// AcceptDispatch 回收员接单：其他候选全部置 expired，预约进入 assigned。
func (s *Service) AcceptDispatch(ctx context.Context, recyclerID, apptID int64) error {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return err
	}
	if a.Stage != "booked" && a.Stage != "assigned" {
		return bizError("STAGE_CONFLICT", "该预约已进入后续环节，无法接单")
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `
UPDATE dispatches SET status='accepted', responded_at=now()
WHERE appointment_id=$1 AND recycler_id=$2 AND status IN ('offered')`,
			apptID, recyclerID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("DISPATCH_NOT_OFFERED", "没有待您接单的派单（可能已被他人接单或已超时）")
		}
		if _, err := t.Exec(ctx, `
UPDATE dispatches SET status='expired', responded_at=now()
WHERE appointment_id=$1 AND recycler_id<>$2 AND status='offered'`, apptID, recyclerID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET stage='assigned', updated_at=now() WHERE id=$1`, apptID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `
INSERT INTO visits(appointment_id,recycler_id) VALUES($1,$2)
ON CONFLICT (appointment_id) DO NOTHING`, apptID, recyclerID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageDispatch, "dispatch.accepted",
			&recyclerID, "recycler", nil); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicDispatch, bus.EventTypeDispatchAccepted, map[string]any{
			"appointment_id": apptID, "recycler_id": recyclerID,
		})
	})
}

// DeclineDispatch 回收员拒单；若全部候选都拒绝，等待运营重新派单。
func (s *Service) DeclineDispatch(ctx context.Context, recyclerID, apptID int64, reason string) error {
	return s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `
UPDATE dispatches SET status='declined', responded_at=now()
WHERE appointment_id=$1 AND recycler_id=$2 AND status='offered'`, apptID, recyclerID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("DISPATCH_NOT_OFFERED", "没有待您处理的派单")
		}
		if err := addEvent(ctx, t, apptID, models.StageDispatch, "dispatch.declined",
			&recyclerID, "recycler", map[string]any{"reason": reason}); err != nil {
			return err
		}
		return nil
	})
}

// ManualAssign 运营手工改派（原 accepted/offered 派单全部取消）。
func (s *Service) ManualAssign(ctx context.Context, operatorID, apptID, recyclerID int64) error {
	return s.tx(ctx, func(t pgx.Tx) error {
		var stationID int64
		var rl, ra, al, aa float64
		if err := t.QueryRow(ctx,
			`SELECT station_id,lng,lat FROM recyclers WHERE account_id=$1 AND duty_status='on'`,
			recyclerID).Scan(&stationID, &rl, &ra); err != nil {
			return bizError("RECYCLER_UNAVAILABLE", "回收员不存在或未在岗")
		}
		if err := t.QueryRow(ctx, `SELECT lng,lat FROM appointments WHERE id=$1`, apptID).Scan(&al, &aa); err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`UPDATE dispatches SET status='canceled', responded_at=now()
			 WHERE appointment_id=$1 AND status IN ('offered','accepted')`, apptID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `
INSERT INTO dispatches(appointment_id,recycler_id,station_id,distance_km,status,assigned_by)
VALUES($1,$2,$3,$4,'accepted','manual')
ON CONFLICT (appointment_id,recycler_id)
DO UPDATE SET status='accepted', responded_at=now(), assigned_by='manual', distance_km=EXCLUDED.distance_km`,
			apptID, recyclerID, stationID, pricing.HaversineKm(al, aa, rl, ra)); err != nil {
			return err
		}
		var stage string
		if err := t.QueryRow(ctx, `SELECT stage FROM appointments WHERE id=$1`, apptID).Scan(&stage); err != nil {
			return err
		}
		if stage == "booked" {
			if _, err := t.Exec(ctx,
				`UPDATE appointments SET stage='assigned', updated_at=now() WHERE id=$1`, apptID); err != nil {
				return err
			}
		}
		if _, err := t.Exec(ctx, `
INSERT INTO visits(appointment_id,recycler_id) VALUES($1,$2)
ON CONFLICT (appointment_id) DO UPDATE SET recycler_id=EXCLUDED.recycler_id`,
			apptID, recyclerID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageDispatch, "dispatch.manual_assigned",
			&operatorID, "operator", map[string]any{"recycler_id": recyclerID}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicDispatch, "DispatchManualAssigned", map[string]any{
			"appointment_id": apptID, "recycler_id": recyclerID, "by": operatorID,
		})
	})
}

// Enroute 回收员出发。
func (s *Service) Enroute(ctx context.Context, recyclerID, apptID int64) error {
	return s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `
UPDATE visits SET enroute_at=now()
WHERE appointment_id=$1 AND recycler_id=$2 AND enroute_at IS NULL`, apptID, recyclerID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("VISIT_NOT_FOUND", "未找到您的上门记录")
		}
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET stage='enroute', updated_at=now() WHERE id=$1 AND stage='assigned'`,
			apptID); err != nil {
			return err
		}
		return addEvent(ctx, t, apptID, models.StageDispatch, "dispatch.enroute",
			&recyclerID, "recycler", nil)
	})
}

// ListDispatches 回收员查看自己的派单/接单。
func (s *Service) ListDispatches(ctx context.Context, recyclerID int64, status string) ([]map[string]any, error) {
	q := `
SELECT d.appointment_id, d.recycler_id, d.station_id, d.distance_km, d.status,
       a.code, a.address, a.window_start, a.window_end, a.est_amount, a.est_weight_kg,
       a.promised_arrive_at, a.stage
FROM dispatches d JOIN appointments a ON a.id=d.appointment_id
WHERE d.recycler_id=$1`
	args := []any{recyclerID}
	if status != "" {
		q += " AND d.status=$2"
		args = append(args, status)
	}
	q += " ORDER BY d.id DESC LIMIT 50"
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var apptID, rid, sid int64
		var dist, estAmount, estWeight float64
		var st, code, addr, stage string
		var ws, we time.Time
		var promised *time.Time
		if err := rows.Scan(&apptID, &rid, &sid, &dist, &st, &code, &addr,
			&ws, &we, &estAmount, &estWeight, &promised, &stage); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"appointment_id": apptID, "recycler_id": rid, "station_id": sid,
			"distance_km": dist, "dispatch_status": st, "code": code, "address": addr,
			"window_start": ws, "window_end": we, "est_amount": estAmount,
			"est_weight_kg": estWeight, "promised_arrive_at": promised, "stage": stage,
		})
	}
	return out, nil
}
