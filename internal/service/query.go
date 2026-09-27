package service

import (
	"context"
	"encoding/json"
	"math"

	"recycle/internal/models"
)

// Notifications 居民站内通知（价格/积分/复称/凭证等）。
func (s *Service) Notifications(ctx context.Context, accountID int64, unreadOnly bool) ([]map[string]any, error) {
	q := `SELECT id,COALESCE(appointment_id,0),title,content,read,created_at
FROM notifications WHERE account_id=$1`
	args := []any{accountID}
	if unreadOnly {
		q += " AND read=false"
	}
	q += " ORDER BY id DESC LIMIT 50"
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, apptID int64
		var title, content string
		var read bool
		var createdAt any
		if err := rows.Scan(&id, &apptID, &title, &content, &read, &createdAt); err != nil {
			return nil, err
		}
		row := map[string]any{
			"id": id, "title": title, "content": content,
			"read": read, "created_at": createdAt,
		}
		if apptID != 0 {
			row["appointment_id"] = apptID
		}
		out = append(out, row)
	}
	return out, nil
}

// MarkNotificationRead 居民标记通知已读。
func (s *Service) MarkNotificationRead(ctx context.Context, accountID, notificationID int64) error {
	ct, err := s.db.Exec(ctx,
		`UPDATE notifications SET read=true WHERE id=$1 AND account_id=$2`, notificationID, accountID)
	if err != nil {
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Timeline 返回一次上门回收六环节的完整追踪：谁、何时、确认了什么、对结算的影响。
func (s *Service) Timeline(ctx context.Context, apptID int64) ([]models.StageEvent, error) {
	rows, err := s.db.Query(ctx, `
SELECT id,appointment_id,stage,event_type,actor_id,COALESCE(actor_role,''),payload,created_at
FROM stage_events WHERE appointment_id=$1 ORDER BY id`, apptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.StageEvent
	for rows.Next() {
		var e models.StageEvent
		var payload []byte
		if err := rows.Scan(&e.ID, &e.AppointmentID, &e.Stage, &e.EventType,
			&e.ActorID, &e.ActorRole, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, nil
}

// PointBalance 居民积分余额。
func (s *Service) PointBalance(ctx context.Context, accountID int64) (int64, error) {
	var balance int64
	err := s.db.QueryRow(ctx,
		`SELECT balance FROM point_accounts WHERE account_id=$1`, accountID).Scan(&balance)
	if err != nil {
		return 0, err
	}
	return balance, nil
}

// PointLedger 积分流水。
func (s *Service) PointLedger(ctx context.Context, accountID int64, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `
SELECT id,COALESCE(appointment_id,0),change_points,balance_after,type,COALESCE(remark,''),created_at
FROM point_ledger WHERE account_id=$1 ORDER BY id DESC LIMIT $2`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, apptID, change, after int64
		var typ, remark string
		var createdAt any
		if err := rows.Scan(&id, &apptID, &change, &after, &typ, &remark, &createdAt); err != nil {
			return nil, err
		}
		row := map[string]any{
			"id": id, "change_points": change, "balance_after": after,
			"type": typ, "remark": remark, "created_at": createdAt,
		}
		if apptID != 0 {
			row["appointment_id"] = apptID
		}
		out = append(out, row)
	}
	return out, nil
}

// RecyclerEarnings 回收员收益汇总与路线（路线与收益视角）。
func (s *Service) RecyclerEarnings(ctx context.Context, recyclerID int64) (map[string]any, error) {
	rows, err := s.db.Query(ctx, `
SELECT type, COALESCE(SUM(amount),0), COUNT(*)
FROM earnings_ledger WHERE recycler_id=$1 GROUP BY type`, recyclerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byType := map[string]any{}
	var total float64
	for rows.Next() {
		var typ string
		var amount float64
		var cnt int64
		if err := rows.Scan(&typ, &amount, &cnt); err != nil {
			return nil, err
		}
		byType[typ] = map[string]any{"amount": amount, "count": cnt}
		total += amount
	}

	rrows, err := s.db.Query(ctx, `
SELECT a.id, a.code, a.address, d.distance_km, d.status,
       v.enroute_at, v.arrived_at, v.late_minutes, a.stage,
       COALESCE((SELECT total_amount FROM weigh_records w
                 WHERE w.appointment_id=a.id AND w.phase='onsite'
                 AND w.status IN ('confirmed','adjusted')
                 ORDER BY w.seq DESC LIMIT 1),0)
FROM dispatches d
JOIN appointments a ON a.id=d.appointment_id
LEFT JOIN visits v ON v.appointment_id=a.id
WHERE d.recycler_id=$1 AND d.status='accepted'
ORDER BY COALESCE(v.arrived_at, v.enroute_at, d.offered_at) DESC
LIMIT 20`, recyclerID)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	var route []map[string]any
	for rrows.Next() {
		var id int64
		var code, addr, dstatus, stage string
		var dist, amount float64
		var enroute, arrived any
		var late int
		if err := rrows.Scan(&id, &code, &addr, &dist, &dstatus,
			&enroute, &arrived, &late, &stage, &amount); err != nil {
			return nil, err
		}
		route = append(route, map[string]any{
			"appointment_id": id, "code": code, "address": addr, "distance_km": dist,
			"enroute_at": enroute, "arrived_at": arrived, "late_minutes": late,
			"stage": stage, "settled_amount": amount,
		})
	}
	return map[string]any{
		"total_amount": math.Round(total*100) / 100,
		"by_type":      byType,
		"route":        route,
	}, nil
}

// AppointmentDetail 聚合：预约+派单+上门+称重+结算+入站+凭证+争议+时间线。
func (s *Service) AppointmentDetail(ctx context.Context, apptID int64) (map[string]any, error) {
	a, err := s.GetAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"appointment": a}

	if dispatches, err := s.dispatchSummaries(ctx, apptID); err == nil {
		out["dispatches"] = dispatches
	}
	if v, err := s.visitSummary(ctx, apptID); err == nil {
		out["visit"] = v
	}
	if w, err := s.weighSummaries(ctx, apptID); err == nil {
		out["weigh_records"] = w
	}
	if ls, err := s.ListSettlements(ctx, apptID); err == nil {
		out["settlements"] = ls
	}
	if ib, err := s.inboundSummary(ctx, apptID); err == nil {
		out["inbound"] = ib
	}
	if c, err := s.GetCertificateByAppointment(ctx, apptID); err == nil {
		out["certificate"] = c
	}
	if disputes, err := s.disputesOf(ctx, apptID); err == nil {
		out["disputes"] = disputes
	}
	if tl, err := s.Timeline(ctx, apptID); err == nil {
		out["timeline"] = tl
	}
	return out, nil
}

func (s *Service) dispatchSummaries(ctx context.Context, apptID int64) ([]map[string]any, error) {
	rows, err := s.db.Query(ctx, `
SELECT id,recycler_id,station_id,distance_km,status,assigned_by,offered_at,responded_at
FROM dispatches WHERE appointment_id=$1 ORDER BY id`, apptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, rid, sid int64
		var dist float64
		var st, by string
		var offered, responded any
		if err := rows.Scan(&id, &rid, &sid, &dist, &st, &by, &offered, &responded); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id": id, "recycler_id": rid, "station_id": sid, "distance_km": dist,
			"status": st, "assigned_by": by, "offered_at": offered, "responded_at": responded,
		})
	}
	return out, nil
}

func (s *Service) visitSummary(ctx context.Context, apptID int64) (map[string]any, error) {
	var recyclerID int64
	var enroute, arrived, verified any
	var late int
	var method, note string
	var verifiedBy *int64
	if err := s.db.QueryRow(ctx, `
SELECT recycler_id,enroute_at,arrived_at,late_minutes,verified_at,COALESCE(verify_method,''),
       verified_by,COALESCE(assist_note,'')
FROM visits WHERE appointment_id=$1`, apptID).Scan(
		&recyclerID, &enroute, &arrived, &late, &verified, &method, &verifiedBy, &note); err != nil {
		return nil, err
	}
	return map[string]any{
		"recycler_id": recyclerID, "enroute_at": enroute, "arrived_at": arrived,
		"late_minutes": late, "verified_at": verified, "verify_method": method,
		"verified_by": verifiedBy, "assist_note": note,
	}, nil
}

func (s *Service) weighSummaries(ctx context.Context, apptID int64) ([]map[string]any, error) {
	rows, err := s.db.Query(ctx, `
SELECT w.id,w.phase,w.seq,w.total_weight_kg,w.total_amount,w.status,
       COALESCE(w.confirm_method,''),w.confirmed_at,
       (SELECT COALESCE(json_agg(json_build_object(
          'category',wi.category,'weight_kg',wi.weight_kg,'rejected_kg',wi.rejected_kg,
          'unit_price',wi.unit_price,'amount',wi.amount,
          'rejected_reason',wi.rejected_reason)),'[]'::json)
        FROM weigh_items wi WHERE wi.weigh_record_id=w.id)
FROM weigh_records w WHERE w.appointment_id=$1 ORDER BY w.phase,w.seq`, apptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, seq int64
		var phase, status, method string
		var weight, amount float64
		var confirmedAt any
		var items []byte
		if err := rows.Scan(&id, &phase, &seq, &weight, &amount, &status,
			&method, &confirmedAt, &items); err != nil {
			return nil, err
		}
		var parsedItems any
		_ = json.Unmarshal(items, &parsedItems)
		out = append(out, map[string]any{
			"id": id, "phase": phase, "seq": seq, "total_weight_kg": weight,
			"total_amount": amount, "status": status, "confirm_method": method,
			"confirmed_at": confirmedAt, "items": parsedItems,
		})
	}
	return out, nil
}

func (s *Service) inboundSummary(ctx context.Context, apptID int64) (map[string]any, error) {
	var id, sid, ow, sw int64
	var onsiteW, stationW, diffKg, diffPct, rejected, adjAmount float64
	var adjPoints int64
	var status string
	err := s.db.QueryRow(ctx, `
SELECT id,station_id,onsite_weigh_id,station_weigh_id,
       onsite_weight_kg,station_weight_kg,diff_kg,diff_pct,rejected_kg,
       adjustment_amount,adjustment_points,status
FROM inbound_records WHERE appointment_id=$1`, apptID).Scan(
		&id, &sid, &ow, &sw, &onsiteW, &stationW, &diffKg, &diffPct, &rejected,
		&adjAmount, &adjPoints, &status)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": id, "station_id": sid, "onsite_weigh_id": ow, "station_weigh_id": sw,
		"onsite_weight_kg": onsiteW, "station_weight_kg": stationW,
		"diff_kg": diffKg, "diff_pct": diffPct, "rejected_kg": rejected,
		"adjustment_amount": adjAmount, "adjustment_points": adjPoints, "status": status,
	}, nil
}

func (s *Service) disputesOf(ctx context.Context, apptID int64) ([]models.Dispute, error) {
	rows, err := s.db.Query(ctx, `
SELECT id,appointment_id,type,raised_by,title,COALESCE(description,''),status,
       COALESCE(resolution,''),adjust_amount,adjust_points,resolver_id,created_at,resolved_at
FROM disputes WHERE appointment_id=$1 ORDER BY id`, apptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDisputes(rows)
}
