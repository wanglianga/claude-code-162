package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"recycle/internal/bus"
	"recycle/internal/models"
)

// Arrival 回收员到达。自动计算迟到分钟数（相对承诺到达时间）。
func (s *Service) Arrival(ctx context.Context, recyclerID, apptID int64) (map[string]any, error) {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	lateMin := 0
	if a.PromisedArriveAt != nil && time.Now().After(*a.PromisedArriveAt) {
		lateMin = int(time.Since(*a.PromisedArriveAt).Minutes())
	}
	err = s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `
UPDATE visits SET arrived_at=now(), late_minutes=$3
WHERE appointment_id=$1 AND recycler_id=$2 AND arrived_at IS NULL`,
			apptID, recyclerID, lateMin)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("VISIT_NOT_FOUND", "未找到您的上门记录")
		}
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET stage='arrived', updated_at=now() WHERE id=$1 AND stage IN ('assigned','enroute')`,
			apptID); err != nil {
			return err
		}
		payload := map[string]any{"late_minutes": lateMin}
		evt := "visit.arrived"
		if lateMin > 0 {
			evt = "visit.arrived_late"
			payload["promised_arrive_at"] = a.PromisedArriveAt
			payload["handling_hint"] = "回收员迟到，系统已记录；居民可发起迟到争议"
		}
		if err := addEvent(ctx, t, apptID, models.StageDispatch, evt,
			&recyclerID, "recycler", payload); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"appointment_id": apptID, "late_minutes": lateMin}, nil
}

// VerifyInput 现场身份核验。
type VerifyInput struct {
	QRToken string `json:"qr_token"` // 居民出示预约二维码
	Method  string `json:"method"`   // qr_scan / manual / assist
	Note    string `json:"note"`     // assist 时必填（谁授权、为何代确认）
}

// VerifyResident 扫码确认居民身份。老人不会确认时 method=assist，
// 由回收员代扫码/坐席协助，必须留痕（assist_note + verified_by）。
func (s *Service) VerifyResident(ctx context.Context, recyclerID, apptID int64, in VerifyInput) error {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return err
	}
	if a.QrToken != in.QRToken {
		return bizError("QR_MISMATCH", "二维码与预约单不匹配，已拒绝上门核验")
	}
	if in.Method == "" {
		in.Method = "qr_scan"
	}
	if in.Method != "qr_scan" && in.Method != "manual" && in.Method != "assist" {
		return bizError("METHOD_INVALID", "核验方式只能是 qr_scan/manual/assist")
	}
	var elderly bool
	if err := s.db.QueryRow(ctx, `SELECT elderly FROM residents WHERE account_id=$1`, a.ResidentID).Scan(&elderly); err != nil {
		return err
	}
	if in.Method == "assist" && in.Note == "" {
		return bizError("ASSIST_NOTE_REQUIRED", "代确认必须填写授权说明（谁同意、以何种方式确认）")
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `
UPDATE visits SET verified_at=now(), verify_method=$3, verified_by=$4, assist_note=$5
WHERE appointment_id=$1 AND recycler_id=$2`,
			apptID, recyclerID, in.Method, recyclerID, in.Note)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("VISIT_NOT_FOUND", "未找到上门记录")
		}
		payload := map[string]any{
			"method": in.Method, "verified_by": recyclerID, "resident_elderly": elderly,
		}
		if in.Method == "assist" {
			payload["assist_note"] = in.Note
			payload["explanation"] = "老人无法自行确认，由回收员/坐席协助完成，全过程留痕可追溯"
		}
		if err := addEvent(ctx, t, apptID, models.StageOnsite, "visit.resident_verified",
			&recyclerID, "recycler", payload); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicOnsite, bus.EventTypeVisitVerified, map[string]any{
			"appointment_id": apptID, "method": in.Method, "verified_by": recyclerID,
			"resident_id": a.ResidentID, "elderly": elderly,
		})
	})
}

// QRCode 返回预约二维码令牌（居民端展示，演示用字符串）。
func (s *Service) QRCode(ctx context.Context, residentID, apptID int64) (string, error) {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", ErrNotFound
		}
		return "", err
	}
	if a.ResidentID != residentID {
		return "", ErrForbidden
	}
	return a.QrToken, nil
}
