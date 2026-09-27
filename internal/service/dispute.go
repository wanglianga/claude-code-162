package service

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"recycle/internal/bus"
	"recycle/internal/models"
)

// ResolveDisputeInput 运营处理争议，可给出金额/积分补偿。
type ResolveDisputeInput struct {
	Resolution   string  `json:"resolution"`
	AdjustAmount float64 `json:"adjust_amount"` // 正数补偿居民
	AdjustPoints int64   `json:"adjust_points"`
}

// RaiseDispute 通用争议上报（迟到/品类/结算等）。
func (s *Service) RaiseDispute(ctx context.Context, apptID, raisedBy int64, dtype, title, desc string) (int64, error) {
	valid := map[string]bool{"weight": true, "category": true, "settlement": true,
		"late": true, "contamination": true, "other": true}
	if !valid[dtype] {
		return 0, bizError("TYPE_INVALID", "争议类型不合法")
	}
	var id int64
	err := s.tx(ctx, func(t pgx.Tx) error {
		if err := t.QueryRow(ctx, `
INSERT INTO disputes(appointment_id,type,raised_by,title,description,status)
VALUES($1,$2,$3,$4,$5,'open') RETURNING id`,
			apptID, dtype, raisedBy, title, desc).Scan(&id); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, "dispute", "dispute.raised",
			&raisedBy, "unknown", map[string]any{
				"dispute_id": id, "type": dtype, "title": title, "description": desc,
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicDispute, bus.EventTypeDisputeRaised, map[string]any{
			"appointment_id": apptID, "dispute_id": id, "type": dtype, "raised_by": raisedBy,
		})
	})
	return id, err
}

// ResolveDispute 运营复核并处理争议：补偿以积分入账（幂等），全程留痕。
func (s *Service) ResolveDispute(ctx context.Context, resolverID, disputeID int64, in ResolveDisputeInput) error {
	if in.Resolution == "" {
		return bizError("RESOLUTION_REQUIRED", "处理结论必填")
	}
	var d models.Dispute
	var apptID int64
	err := s.db.QueryRow(ctx,
		`SELECT id,appointment_id,type,status FROM disputes WHERE id=$1`, disputeID).
		Scan(&d.ID, &apptID, &d.Type, &d.Status)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if d.Status == "resolved" || d.Status == "closed" {
		return bizError("DISPUTE_DONE", "争议已处理完成")
	}
	var residentID int64
	if err := s.db.QueryRow(ctx, `SELECT resident_id FROM appointments WHERE id=$1`, apptID).Scan(&residentID); err != nil {
		return err
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		if in.AdjustPoints > 0 {
			key := fmt.Sprintf("points:%d:dispute:%d", apptID, disputeID)
			if _, err := t.Exec(ctx, `
INSERT INTO point_ledger(account_id,appointment_id,change_points,balance_after,type,idempotency_key,remark)
SELECT $1,$2,$3,balance+$3,'compensation',$4,$5
FROM point_accounts WHERE account_id=$1
ON CONFLICT (idempotency_key) DO NOTHING`,
				residentID, apptID, in.AdjustPoints, key,
				fmt.Sprintf("争议#%d 补偿积分", disputeID)); err != nil {
				return err
			}
			if _, err := t.Exec(ctx,
				`UPDATE point_accounts SET balance=balance+$2, version=version+1 WHERE account_id=$1`,
				residentID, in.AdjustPoints); err != nil {
				return err
			}
		}
		if in.AdjustAmount > 0 {
			key := fmt.Sprintf("settle:%d:dispute:%d", apptID, disputeID)
			// 现金补偿也记录一笔待兑付结算（站点备用金场景简化为记录），演示用积分等价补偿
			pts := int64(math.Round(in.AdjustAmount * 100))
			if _, err := t.Exec(ctx, `
INSERT INTO settlements(appointment_id,kind,method,amount,points,status,idempotency_key,settled_by,settled_at)
VALUES($1,'adjustment','points',$2,$3,'succeeded',$4,$5,now())
ON CONFLICT (idempotency_key) DO NOTHING`,
				apptID, in.AdjustAmount, pts, key, resolverID); err != nil {
				return err
			}
		}
		if _, err := t.Exec(ctx, `
UPDATE disputes SET status='resolved',resolution=$2,adjust_amount=$3,adjust_points=$4,
       resolver_id=$5,resolved_at=now() WHERE id=$1`,
			disputeID, in.Resolution, in.AdjustAmount, in.AdjustPoints, resolverID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, "dispute", "dispute.resolved",
			&resolverID, "operator", map[string]any{
				"dispute_id": disputeID, "type": d.Type, "resolution": in.Resolution,
				"adjust_amount": in.AdjustAmount, "adjust_points": in.AdjustPoints,
				"impact": "补偿以积分幂等入账，相关结算影响已记录",
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicDispute, bus.EventTypeDisputeResolved, map[string]any{
			"appointment_id": apptID, "dispute_id": disputeID, "resolver_id": resolverID,
			"adjust_points": in.AdjustPoints, "adjust_amount": in.AdjustAmount,
		})
	})
}

// ListDisputes 列出争议（运营全部，其他人按预约）。
func (s *Service) ListDisputes(ctx context.Context, status string) ([]models.Dispute, error) {
	q := `SELECT id,appointment_id,type,raised_by,title,COALESCE(description,''),status,
       COALESCE(resolution,''),adjust_amount,adjust_points,resolver_id,created_at,resolved_at
FROM disputes`
	args := []any{}
	if status != "" {
		q += " WHERE status=$1"
		args = append(args, status)
	}
	q += " ORDER BY id DESC LIMIT 100"
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDisputes(rows)
}

func scanDisputes(rows interface {
	Next() bool
	Scan(...any) error
	Close()
}) ([]models.Dispute, error) {
	var out []models.Dispute
	for rows.Next() {
		var d models.Dispute
		if err := rows.Scan(&d.ID, &d.AppointmentID, &d.Type, &d.RaisedBy, &d.Title,
			&d.Description, &d.Status, &d.Resolution, &d.AdjustAmount, &d.AdjustPoints,
			&d.ResolverID, &d.CreatedAt, &d.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// ---------- 投诉 ----------

func (s *Service) CreateComplaint(ctx context.Context, complainantID int64, apptID *int64, category, content string) (int64, error) {
	if content == "" {
		return 0, bizError("CONTENT_REQUIRED", "投诉内容必填")
	}
	var id int64
	err := s.tx(ctx, func(t pgx.Tx) error {
		if err := t.QueryRow(ctx, `
INSERT INTO complaints(appointment_id,complainant_id,category,content)
VALUES($1,$2,$3,$4) RETURNING id`,
			apptID, complainantID, category, content).Scan(&id); err != nil {
			return err
		}
		stage := "complaint"
		if apptID != nil {
			return addEvent(ctx, t, *apptID, stage, "complaint.created",
				&complainantID, "resident", map[string]any{"complaint_id": id, "category": category})
		}
		return nil
	})
	return id, err
}

func (s *Service) HandleComplaint(ctx context.Context, handlerID, complaintID int64, response string) error {
	if response == "" {
		return bizError("RESPONSE_REQUIRED", "处理回复必填")
	}
	var apptID *int64
	err := s.db.QueryRow(ctx, `SELECT appointment_id FROM complaints WHERE id=$1`, complaintID).Scan(&apptID)
	if err == pgx.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `
UPDATE complaints SET status='closed',response=$2,handler_id=$3,handled_at=now()
WHERE id=$1 AND status<>'closed'`, complaintID, response, handlerID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("COMPLAINT_CLOSED", "投诉已处理")
		}
		if apptID != nil {
			return addEvent(ctx, t, *apptID, "complaint", "complaint.handled",
				&handlerID, "operator", map[string]any{"complaint_id": complaintID, "response": response})
		}
		return nil
	})
}
