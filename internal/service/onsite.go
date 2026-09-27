package service

import (
	"context"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"recycle/internal/bus"
	"recycle/internal/models"
	"recycle/internal/pricing"
)

type WeighItemInput struct {
	Category       string   `json:"category"`
	WeightKg       float64  `json:"weight_kg"`
	UnitPrice      float64  `json:"unit_price"`  // 可选；缺省用当前市价
	RejectedKg     float64  `json:"rejected_kg"` // 混入不可回收物重量（拒收）
	RejectedReason string   `json:"rejected_reason"`
	PhotoURLs      []string `json:"photo_urls"`
}

type WeighInput struct {
	Items   []WeighItemInput `json:"items"`
	ScaleNo string           `json:"scale_no"`
	Note    string           `json:"note"`
}

type catPrice struct {
	name      string
	price     float64
	pointsPer float64
	co2       float64
}

func (s *Service) loadPrices(ctx context.Context, t pgx.Tx) (map[string]catPrice, error) {
	rows, err := t.Query(ctx,
		`SELECT code,name,unit_price,points_per_yuan,co2_per_kg FROM categories WHERE active=true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]catPrice{}
	for rows.Next() {
		var c catPrice
		var code string
		if err := rows.Scan(&code, &c.name, &c.price, &c.pointsPer, &c.co2); err != nil {
			return nil, err
		}
		out[code] = c
	}
	return out, nil
}

// OnsiteWeigh 回收员现场称重定价：
// 必须已核验居民身份；记录实测品类、重量、拒收（混装）、现场单价与照片。
// 结果先为 pending，需居民确认；有争议时可重新称重。
func (s *Service) OnsiteWeigh(ctx context.Context, recyclerID, apptID int64, in WeighInput) (*models.WeighRecord, error) {
	if len(in.Items) == 0 {
		return nil, bizError("ITEMS_EMPTY", "至少称重一个品类")
	}
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	if a.Stage != "arrived" && a.Stage != "priced" {
		return nil, bizError("STAGE_CONFLICT", "请先完成上门与居民身份核验，再现场称重")
	}

	var verified bool
	err = s.db.QueryRow(ctx,
		`SELECT verified_at IS NOT NULL FROM visits WHERE appointment_id=$1`, apptID).Scan(&verified)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, bizError("NOT_VERIFIED", "请先扫码确认居民身份后再称重")
	}
	// 已确认过的称重不可覆盖（争议后重新称重会作废旧单）
	var activeCnt int
	if err := s.db.QueryRow(ctx,
		`SELECT count(*) FROM weigh_records WHERE appointment_id=$1 AND phase='onsite' AND status IN ('confirmed','adjusted')`,
		apptID).Scan(&activeCnt); err != nil {
		return nil, err
	}
	if activeCnt > 0 {
		return nil, bizError("WEIGH_CONFIRMED", "现场称重已确认并结算，不能修改；如确有误差请走站点复称/争议")
	}

	var recordID int64
	err = s.tx(ctx, func(t pgx.Tx) error {
		prices, err := s.loadPrices(ctx, t)
		if err != nil {
			return err
		}
		var seq int
		if err := t.QueryRow(ctx,
			`SELECT COALESCE(max(seq),0)+1 FROM weigh_records WHERE appointment_id=$1 AND phase='onsite'`,
			apptID).Scan(&seq); err != nil {
			return err
		}
		// 上一张争议单作废
		if _, err := t.Exec(ctx,
			`UPDATE weigh_records SET status='void' WHERE appointment_id=$1 AND phase='onsite' AND status IN ('pending','disputed')`,
			apptID); err != nil {
			return err
		}

		var totalWeight, totalAmount float64
		var linePayload []map[string]any
		var rejectedTotal float64
		type readyItem struct {
			models.WeighItem
			pointsPer float64
		}
		var ready []readyItem
		for _, it := range in.Items {
			cp, ok := prices[it.Category]
			if !ok {
				return bizError("CATEGORY_UNKNOWN", "未知或已停收品类: "+it.Category)
			}
			if it.WeightKg <= 0 {
				return bizError("WEIGHT_INVALID", cp.name+" 重量必须大于 0")
			}
			if it.RejectedKg > it.WeightKg {
				return bizError("REJECTED_INVALID", cp.name+" 拒收重量不能超过总重量")
			}
			if it.UnitPrice <= 0 {
				it.UnitPrice = cp.price
			}
			billable := it.WeightKg - it.RejectedKg
			amount := pricing.SettleAmount(billable, it.UnitPrice)
			totalWeight += it.WeightKg
			totalAmount += amount
			rejectedTotal += it.RejectedKg
			ready = append(ready, readyItem{
				WeighItem: models.WeighItem{
					Category: it.Category, WeightKg: it.WeightKg, UnitPrice: it.UnitPrice,
					Amount: amount, RejectedKg: it.RejectedKg, RejectedReason: it.RejectedReason,
					PhotoURLs: it.PhotoURLs,
				},
				pointsPer: cp.pointsPer,
			})
			deviation := 0.0
			if cp.price > 0 {
				deviation = math.Round((it.UnitPrice/cp.price-1)*1000) / 10
			}
			linePayload = append(linePayload, map[string]any{
				"category": it.Category, "name": cp.name, "weight_kg": it.WeightKg,
				"billable_kg": billable, "rejected_kg": it.RejectedKg,
				"rejected_reason": it.RejectedReason, "unit_price": it.UnitPrice,
				"market_price": cp.price, "price_deviation_pct": deviation, "amount": amount,
			})
		}
		totalWeight = math.Round(totalWeight*1000) / 1000
		totalAmount = math.Round(totalAmount*100) / 100

		if err := t.QueryRow(ctx, `
INSERT INTO weigh_records(appointment_id,phase,seq,weighed_by,scale_no,total_weight_kg,total_amount,status,note)
VALUES($1,'onsite',$2,$3,$4,$5,$6,'pending',$7) RETURNING id`,
			apptID, seq, recyclerID, in.ScaleNo, totalWeight, totalAmount, in.Note).Scan(&recordID); err != nil {
			return err
		}
		for _, ri := range ready {
			photos := ri.PhotoURLs
			if photos == nil {
				photos = []string{}
			}
			if _, err := t.Exec(ctx, `
INSERT INTO weigh_items(weigh_record_id,category,weight_kg,unit_price,amount,rejected_kg,rejected_reason,photo_urls)
VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
				recordID, ri.Category, ri.WeightKg, ri.UnitPrice, ri.Amount,
				ri.RejectedKg, ri.RejectedReason, photos); err != nil {
				return err
			}
		}
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET stage='priced', updated_at=now() WHERE id=$1`, apptID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageOnsite, "onsite.weighed",
			&recyclerID, "recycler", map[string]any{
				"weigh_record_id": recordID, "seq": seq, "scale_no": in.ScaleNo,
				"total_weight_kg": totalWeight, "total_amount": totalAmount,
				"rejected_kg": rejectedTotal, "items": linePayload,
				"note": in.Note,
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicOnsite, bus.EventTypeWeighConfirmed, map[string]any{
			"appointment_id": apptID, "weigh_record_id": recordID, "phase": "onsite",
			"total_weight_kg": totalWeight, "total_amount": totalAmount,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.GetWeighRecord(ctx, recordID)
}

// GetWeighRecord 读取称重单及品类行。
func (s *Service) GetWeighRecord(ctx context.Context, id int64) (*models.WeighRecord, error) {
	var w models.WeighRecord
	err := s.db.QueryRow(ctx, `
SELECT id,appointment_id,phase,seq,station_id,weighed_by,COALESCE(scale_no,''),total_weight_kg,total_amount,
       status,confirmed_by,COALESCE(confirm_method,''),confirmed_at,COALESCE(note,''),created_at
FROM weigh_records WHERE id=$1`, id).Scan(
		&w.ID, &w.AppointmentID, &w.Phase, &w.Seq, &w.StationID, &w.WeighedBy, &w.ScaleNo,
		&w.TotalWeightKg, &w.TotalAmount, &w.Status, &w.ConfirmedBy, &w.ConfirmMethod,
		&w.ConfirmedAt, &w.Note, &w.CreatedAt)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT id,category,weight_kg,unit_price,amount,rejected_kg,COALESCE(rejected_reason,''),photo_urls
FROM weigh_items WHERE weigh_record_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var wi models.WeighItem
		var photos []string
		if err := rows.Scan(&wi.ID, &wi.Category, &wi.WeightKg, &wi.UnitPrice, &wi.Amount,
			&wi.RejectedKg, &wi.RejectedReason, &photos); err != nil {
			return nil, err
		}
		wi.PhotoURLs = photos
		w.Items = append(w.Items, wi)
	}
	return &w, nil
}

// latestOnsiteWeigh 取该预约当前有效的现场称重单。
func (s *Service) latestOnsiteWeigh(ctx context.Context, apptID int64, includePending bool) (*models.WeighRecord, error) {
	var id int64
	q := `SELECT id FROM weigh_records WHERE appointment_id=$1 AND phase='onsite'`
	if includePending {
		q += ` AND status IN ('pending','confirmed','adjusted','disputed')`
	} else {
		q += ` AND status IN ('confirmed','adjusted')`
	}
	q += ` ORDER BY seq DESC LIMIT 1`
	err := s.db.QueryRow(ctx, q, apptID).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, bizError("WEIGH_NOT_FOUND", "还没有现场称重记录")
	}
	if err != nil {
		return nil, err
	}
	return s.GetWeighRecord(ctx, id)
}

// ConfirmWeigh 居民确认称重结果。agree=false 表示对称重有争议，阻断结算。
func (s *Service) ConfirmWeigh(ctx context.Context, residentID, apptID int64, agree bool, method, note string) error {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return err
	}
	if a.ResidentID != residentID {
		return ErrForbidden
	}
	w, err := s.latestOnsiteWeigh(ctx, apptID, true)
	if err != nil {
		return err
	}
	if w.Status != "pending" && w.Status != "disputed" {
		return bizError("WEIGH_NOT_PENDING", "该称重单已是 "+w.Status+" 状态")
	}
	if method == "" {
		method = "self"
	}
	if method != "self" && method != "assist" {
		return bizError("METHOD_INVALID", "确认方式只能是 self/assist")
	}
	if !agree {
		return s.tx(ctx, func(t pgx.Tx) error {
			if _, err := t.Exec(ctx,
				`UPDATE weigh_records SET status='disputed' WHERE id=$1`, w.ID); err != nil {
				return err
			}
			if _, err := t.Exec(ctx, `
INSERT INTO disputes(appointment_id,type,raised_by,title,description,status)
VALUES($1,'weight',$2,'居民对称重结果有异议',$3,'open')`,
				apptID, residentID, note); err != nil {
				return err
			}
			if err := addEvent(ctx, t, apptID, models.StageOnsite, "onsite.weigh_disputed",
				&residentID, "resident", map[string]any{
					"weigh_record_id": w.ID, "onsite_weight_kg": w.TotalWeightKg,
					"onsite_amount": w.TotalAmount, "note": note,
					"impact": "结算挂起，需重新称重或运营复核后才能继续",
				}); err != nil {
				return err
			}
			return addOutbox(ctx, t, apptRef(apptID), bus.TopicDispute, bus.EventTypeDisputeRaised, map[string]any{
				"appointment_id": apptID, "type": "weight", "raised_by": residentID,
			})
		})
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `
UPDATE weigh_records SET status='confirmed', confirmed_by=$2, confirm_method=$3, confirmed_at=now()
WHERE id=$1`, w.ID, residentID, method); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageOnsite, "onsite.weigh_confirmed",
			&residentID, "resident", map[string]any{
				"weigh_record_id": w.ID, "method": method,
				"total_weight_kg": w.TotalWeightKg, "total_amount": w.TotalAmount,
				"impact": "以该重量与金额进入现金/积分结算",
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicOnsite, "WeighAccepted", map[string]any{
			"appointment_id": apptID, "weigh_record_id": w.ID,
		})
	})
}

// AssistConfirmWeigh 老人不会用手机：回收员当面征得同意后代确认，强制留痕。
func (s *Service) AssistConfirmWeigh(ctx context.Context, recyclerID, apptID int64, note string) error {
	if note == "" {
		return bizError("ASSIST_NOTE_REQUIRED", "代确认必须记录居民知情同意说明")
	}
	w, err := s.latestOnsiteWeigh(ctx, apptID, true)
	if err != nil {
		return err
	}
	if w.Status != "pending" && w.Status != "disputed" {
		return bizError("WEIGH_NOT_PENDING", "该称重单已是 "+w.Status+" 状态")
	}
	var residentID int64
	if err := s.db.QueryRow(ctx, `SELECT resident_id FROM appointments WHERE id=$1`, apptID).Scan(&residentID); err != nil {
		return err
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		if _, err := t.Exec(ctx, `
UPDATE weigh_records SET status='confirmed', confirmed_by=$2, confirm_method='assist', confirmed_at=now()
WHERE id=$1`, w.ID, recyclerID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageOnsite, "onsite.weigh_confirmed_assist",
			&recyclerID, "recycler", map[string]any{
				"weigh_record_id": w.ID, "resident_id": residentID,
				"assist_note": note,
				"impact":      "代确认后进入结算，确认人记录为回收员，后续可追溯",
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicOnsite, "WeighAcceptedAssist", map[string]any{
			"appointment_id": apptID, "weigh_record_id": w.ID, "confirmed_by": recyclerID,
		})
	})
}

type SettleInput struct {
	Method       string `json:"method"`        // cash / points
	SimulateFail bool   `json:"simulate_fail"` // 演示：模拟现金支付失败
}

// Settle 现场结算。现金失败可重试；积分通过台账+幂等键保证不重复到账。
func (s *Service) Settle(ctx context.Context, residentID, apptID int64, in SettleInput) (*models.Settlement, error) {
	if in.Method != "cash" && in.Method != "points" {
		return nil, bizError("METHOD_INVALID", "结算方式只能是 cash/points")
	}
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	if a.ResidentID != residentID {
		return nil, ErrForbidden
	}
	w, err := s.latestOnsiteWeigh(ctx, apptID, false)
	if err != nil {
		return nil, err
	}
	// 已结算过：幂等返回或失败重试
	var existing models.Settlement
	err = s.db.QueryRow(ctx, `
SELECT id,appointment_id,kind,weigh_record_id,method,amount,points,status,idempotency_key,
       attempts,COALESCE(fail_reason,''),settled_by,settled_at,created_at
FROM settlements WHERE appointment_id=$1 AND kind='initial'`, apptID).Scan(
		&existing.ID, &existing.AppointmentID, &existing.Kind, &existing.WeighRecordID,
		&existing.Method, &existing.Amount, &existing.Points, &existing.Status,
		&existing.IdempotencyKey, &existing.Attempts, &existing.FailReason,
		&existing.SettledBy, &existing.SettledAt, &existing.CreatedAt)
	switch {
	case err == nil && existing.Status == "succeeded":
		return &existing, nil // 幂等：成功不重复执行
	case err != nil && err != pgx.ErrNoRows:
		return nil, err
	}

	// 计算积分（按品类系数）
	rows, err := s.db.Query(ctx, `
SELECT wi.category, wi.weight_kg - wi.rejected_kg, wi.amount, c.points_per_yuan
FROM weigh_items wi JOIN categories c ON c.code=wi.category
WHERE wi.weigh_record_id=$1`, w.ID)
	if err != nil {
		return nil, err
	}
	type line struct {
		Category string  `json:"category"`
		Weight   float64 `json:"billable_kg"`
		Amount   float64 `json:"amount"`
		Points   int64   `json:"points"`
	}
	var lines []line
	var totalPoints int64
	for rows.Next() {
		var l line
		var ppY float64
		if err := rows.Scan(&l.Category, &l.Weight, &l.Amount, &ppY); err != nil {
			rows.Close()
			return nil, err
		}
		l.Points = int64(math.Round(l.Amount * ppY))
		totalPoints += l.Points
		lines = append(lines, l)
	}
	rows.Close()

	// 回收员
	var recyclerID int64
	if err := s.db.QueryRow(ctx,
		`SELECT recycler_id FROM visits WHERE appointment_id=$1`, apptID).Scan(&recyclerID); err != nil {
		return nil, err
	}

	idemKey := fmt.Sprintf("settle:%d:initial", apptID)
	feeKey := fmt.Sprintf("earn:fee:%d:initial", apptID)
	pointsKey := fmt.Sprintf("points:%d:initial", apptID)
	fee := math.Round(w.TotalAmount*0.15*100) / 100 // 回收员服务费 15%

	var settleID int64
	err = s.tx(ctx, func(t pgx.Tx) error {
		if existing.ID == 0 {
			if err := t.QueryRow(ctx, `
INSERT INTO settlements(appointment_id,kind,weigh_record_id,method,amount,points,status,idempotency_key,simulate_fail,settled_by)
VALUES($1,'initial',$2,$3,$4,$5,'pending',$6,$7,$8) RETURNING id`,
				apptID, w.ID, in.Method, w.TotalAmount, totalPoints, idemKey, in.SimulateFail, residentID).Scan(&settleID); err != nil {
				return err
			}
		} else {
			settleID = existing.ID
			if _, err := t.Exec(ctx, `
UPDATE settlements SET status='pending', method=$2, points=$3, simulate_fail=$4,
       attempts=attempts+1, fail_reason=NULL WHERE id=$1`,
				settleID, in.Method, totalPoints, in.SimulateFail); err != nil {
				return err
			}
		}

		// 支付执行前设保存点：支付失败回滚到此处，保留 failed 结算单与事件后提交。
		if _, err := t.Exec(ctx, "SAVEPOINT pay_exec"); err != nil {
			return err
		}
		// ---- 支付执行 ----
		if in.Method == "cash" {
			if in.SimulateFail {
				if err := s.failSettlement(ctx, t, settleID, apptID, residentID, in.Method,
					"现金不足/找零失败（模拟）", recyclerID); err != nil {
					return err
				}
				return nil
			}
			var cashFloat float64
			if err := t.QueryRow(ctx,
				`SELECT cash_float FROM recyclers WHERE account_id=$1 FOR UPDATE`, recyclerID).Scan(&cashFloat); err != nil {
				return err
			}
			if cashFloat < w.TotalAmount {
				if err := s.failSettlement(ctx, t, settleID, apptID, residentID, in.Method,
					fmt.Sprintf("回收员备用金不足：余额 %.2f < 应付 %.2f", cashFloat, w.TotalAmount),
					recyclerID); err != nil {
					return err
				}
				return nil
			}
			if _, err := t.Exec(ctx,
				`UPDATE recyclers SET cash_float = cash_float - $2 WHERE account_id=$1`,
				recyclerID, w.TotalAmount); err != nil {
				return err
			}
		} else {
			// 积分：台账 + 余额同一事务；幂等键兜底重复到账
			if _, err := t.Exec(ctx, `
INSERT INTO point_ledger(account_id,appointment_id,change_points,balance_after,type,idempotency_key,remark)
SELECT $1,$2,$3,balance+$3,'earn',$4,'上门回收结算积分'
FROM point_accounts WHERE account_id=$1
ON CONFLICT (idempotency_key) DO NOTHING`,
				residentID, apptID, totalPoints, pointsKey); err != nil {
				return err
			}
			if _, err := t.Exec(ctx,
				`UPDATE point_accounts SET balance=balance+$2, version=version+1 WHERE account_id=$1`,
				residentID, totalPoints); err != nil {
				return err
			}
		}
		// 回收员服务费（收益）
		if _, err := t.Exec(ctx, `
INSERT INTO earnings_ledger(recycler_id,appointment_id,amount,type,idempotency_key)
VALUES($1,$2,$3,'service_fee',$4) ON CONFLICT (idempotency_key) DO NOTHING`,
			recyclerID, apptID, fee, feeKey); err != nil {
			return err
		}

		if _, err := t.Exec(ctx, `
UPDATE settlements SET status='succeeded', settled_at=now() WHERE id=$1`, settleID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `
UPDATE appointments SET stage='settled', settle_method=$2, updated_at=now() WHERE id=$1`,
			apptID, in.Method); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageSettlement, "settlement.succeeded",
			&residentID, "resident", map[string]any{
				"settlement_id": settleID, "method": in.Method, "amount": w.TotalAmount,
				"points": totalPoints, "lines": lines, "service_fee": fee,
				"recycler_id": recyclerID,
				"impact":      "现金已从回收员备用金兑付/积分已计入居民积分账户，等待入站复称",
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicSettlement, bus.EventTypeSettleSucceeded, map[string]any{
			"appointment_id": apptID, "settlement_id": settleID, "method": in.Method,
			"amount": w.TotalAmount, "points": totalPoints, "resident_id": residentID,
			"recycler_id": recyclerID, "lines": lines, "service_fee": fee,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.getSettlement(ctx, settleID)
}

// failSettlement 回滚支付动作，标记结算失败并留痕（外层事务随后提交失败记录）。
func (s *Service) failSettlement(ctx context.Context, t pgx.Tx, settleID, apptID, residentID int64,
	method, reason string, recyclerID int64) error {
	if _, err := t.Exec(ctx, "ROLLBACK TO SAVEPOINT pay_exec"); err != nil {
		return err
	}
	if _, err := t.Exec(ctx,
		`UPDATE settlements SET status='failed', fail_reason=$2 WHERE id=$1`, settleID, reason); err != nil {
		return err
	}
	if err := addEvent(ctx, t, apptID, models.StageSettlement, "settlement.failed",
		&residentID, "resident", map[string]any{
			"settlement_id": settleID, "method": method, "reason": reason,
			"impact": "结算未完成，可更换方式重试；积分/现金均未发生变动",
		}); err != nil {
		return err
	}
	return addOutbox(ctx, t, apptRef(apptID), bus.TopicSettlement, bus.EventTypeSettleFailed, map[string]any{
		"appointment_id": apptID, "settlement_id": settleID, "method": method,
		"reason": reason, "resident_id": residentID, "recycler_id": recyclerID,
	})
}

func (s *Service) getSettlement(ctx context.Context, id int64) (*models.Settlement, error) {
	var m models.Settlement
	err := s.db.QueryRow(ctx, `
SELECT id,appointment_id,kind,weigh_record_id,method,amount,points,status,idempotency_key,
       attempts,COALESCE(fail_reason,''),settled_by,settled_at,created_at
FROM settlements WHERE id=$1`, id).Scan(
		&m.ID, &m.AppointmentID, &m.Kind, &m.WeighRecordID, &m.Method, &m.Amount,
		&m.Points, &m.Status, &m.IdempotencyKey, &m.Attempts, &m.FailReason,
		&m.SettledBy, &m.SettledAt, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListSettlements 居民查看某预约的结算/调整记录。
func (s *Service) ListSettlements(ctx context.Context, apptID int64) ([]models.Settlement, error) {
	rows, err := s.db.Query(ctx, `
SELECT id,appointment_id,kind,weigh_record_id,method,amount,points,status,idempotency_key,
       attempts,COALESCE(fail_reason,''),settled_by,settled_at,created_at
FROM settlements WHERE appointment_id=$1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Settlement
	for rows.Next() {
		var m models.Settlement
		if err := rows.Scan(&m.ID, &m.AppointmentID, &m.Kind, &m.WeighRecordID, &m.Method,
			&m.Amount, &m.Points, &m.Status, &m.IdempotencyKey, &m.Attempts,
			&m.FailReason, &m.SettledBy, &m.SettledAt, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
