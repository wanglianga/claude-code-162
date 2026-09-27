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

// InboundWeigh 站点复称：回收物入站时按品类再次称重，与现场称重逐品类比对。
// 差异阈值默认 ±5%：
//   - 站点更重（居民少收款）：自动以积分补发差额，并补发回收员服务费；
//   - 站点更轻超阈值：不向居民追回现金，生成重量差异争议，扣减回收员相应服务费，
//     由运营复核；凭证按站点实收重量开具。
func (s *Service) InboundWeigh(ctx context.Context, stationAcctID, apptID int64, in WeighInput) (map[string]any, error) {
	if len(in.Items) == 0 {
		return nil, bizError("ITEMS_EMPTY", "至少复称一个品类")
	}
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	if a.Stage != "settled" && a.Stage != "inbound" {
		return nil, bizError("STAGE_CONFLICT", "只有已现场结算的预约才能入站复称，当前环节: "+a.Stage)
	}
	var stationID int64
	if err := s.db.QueryRow(ctx, `SELECT id FROM stations WHERE account_id=$1`, stationAcctID).Scan(&stationID); err != nil {
		return nil, bizError("NOT_STATION", "当前账号不是回收站账号")
	}
	onsite, err := s.latestOnsiteWeigh(ctx, apptID, false)
	if err != nil {
		return nil, err
	}
	var alreadyInbound bool
	if err := s.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM inbound_records WHERE appointment_id=$1)`, apptID).Scan(&alreadyInbound); err != nil {
		return nil, err
	}
	if alreadyInbound {
		return nil, bizError("ALREADY_INBOUND", "该预约已完成入站复称，差异调整请走争议处理")
	}
	// 现场品类行索引
	onsiteByCat := map[string]models.WeighItem{}
	for _, wi := range onsite.Items {
		onsiteByCat[wi.Category] = wi
	}

	const diffThresholdPct = 5.0
	var stationWeighID, inboundID int64
	result := map[string]any{}

	err = s.tx(ctx, func(t pgx.Tx) error {
		prices, err := s.loadPrices(ctx, t)
		if err != nil {
			return err
		}
		var seq int
		if err := t.QueryRow(ctx,
			`SELECT COALESCE(max(seq),0)+1 FROM weigh_records WHERE appointment_id=$1 AND phase='station'`,
			apptID).Scan(&seq); err != nil {
			return err
		}
		var totalWeight, totalAmount, totalRejected float64
		type stLine struct {
			Category       string  `json:"category"`
			Name           string  `json:"name"`
			Weight         float64 `json:"station_weight_kg"`
			Billable       float64 `json:"billable_kg"`
			Rejected       float64 `json:"rejected_kg"`
			UnitPrice      float64 `json:"unit_price"`
			Amount         float64 `json:"amount"`
			OnsiteWeight   float64 `json:"onsite_weight_kg"`
			OnsiteBillable float64 `json:"onsite_billable_kg"`
			DiffKg         float64 `json:"diff_kg"`
			DiffPct        float64 `json:"diff_pct"`
		}
		var lines []stLine
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
			ol := onsiteByCat[it.Category]
			onBillable := ol.WeightKg - ol.RejectedKg
			diffKg := math.Round((billable-onBillable)*1000) / 1000
			diffPct := 0.0
			if onBillable > 0 {
				diffPct = math.Round((billable/onBillable-1)*10000) / 100
			}
			totalWeight += it.WeightKg
			totalAmount += amount
			totalRejected += it.RejectedKg
			lines = append(lines, stLine{
				Category: it.Category, Name: cp.name, Weight: it.WeightKg,
				Billable: billable, Rejected: it.RejectedKg, UnitPrice: it.UnitPrice,
				Amount: amount, OnsiteWeight: ol.WeightKg, OnsiteBillable: onBillable,
				DiffKg: diffKg, DiffPct: diffPct,
			})
		}
		totalWeight = math.Round(totalWeight*1000) / 1000
		totalAmount = math.Round(totalAmount*100) / 100
		totalRejected = math.Round(totalRejected*1000) / 1000

		if err := t.QueryRow(ctx, `
INSERT INTO weigh_records(appointment_id,phase,seq,station_id,weighed_by,scale_no,total_weight_kg,total_amount,status,
       confirmed_by,confirm_method,confirmed_at,note)
VALUES($1,'station',$2,$3,$4,$5,$6,$7,'confirmed',$3,'review',now(),$8)
RETURNING id`,
			apptID, seq, stationID, stationAcctID, in.ScaleNo, totalWeight, totalAmount, in.Note).Scan(&stationWeighID); err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := t.Exec(ctx, `
INSERT INTO weigh_items(weigh_record_id,category,weight_kg,unit_price,amount,rejected_kg,rejected_reason,photo_urls)
VALUES($1,$2,$3,$4,$5,$6,$7,'{}')`,
				stationWeighID, l.Category, l.Weight, l.UnitPrice, l.Amount,
				l.Rejected, in.Note); err != nil {
				return err
			}
		}

		diffKg := math.Round((totalWeight-onsite.TotalWeightKg)*1000) / 1000
		diffPct := 0.0
		if onsite.TotalWeightKg > 0 {
			diffPct = math.Round((totalWeight/onsite.TotalWeightKg-1)*10000) / 100
		}
		adjustAmount := math.Round((totalAmount-onsite.TotalAmount)*100) / 100
		status := "normal"
		if math.Abs(diffPct) > diffThresholdPct || totalRejected > 0 {
			status = "adjusted"
		}

		var adjustPoints int64
		var recyclerID int64
		if err := t.QueryRow(ctx, `SELECT recycler_id FROM visits WHERE appointment_id=$1`, apptID).Scan(&recyclerID); err != nil {
			return err
		}
		var residentID int64
		if err := t.QueryRow(ctx, `SELECT resident_id FROM appointments WHERE id=$1`, apptID).Scan(&residentID); err != nil {
			return err
		}

		if status == "adjusted" {
			if adjustAmount > 0 {
				// 站点更重：积分补发差额给居民（幂等）
				adjustPoints = int64(math.Round(adjustAmount * 100))
				pointsKey := fmt.Sprintf("points:%d:inbound:add", apptID)
				settleKey := fmt.Sprintf("settle:%d:adjustment:add", apptID)
				if _, err := t.Exec(ctx, `
INSERT INTO point_ledger(account_id,appointment_id,change_points,balance_after,type,idempotency_key,remark)
SELECT $1,$2,$3,balance+$3,'adjust_add',$4,'站点复称补差（实收更重）'
FROM point_accounts WHERE account_id=$1
ON CONFLICT (idempotency_key) DO NOTHING`,
					residentID, apptID, adjustPoints, pointsKey); err != nil {
					return err
				}
				if _, err := t.Exec(ctx,
					`UPDATE point_accounts SET balance=balance+$2, version=version+1 WHERE account_id=$1`,
					residentID, adjustPoints); err != nil {
					return err
				}
				if _, err := t.Exec(ctx, `
INSERT INTO settlements(appointment_id,kind,weigh_record_id,method,amount,points,status,idempotency_key,settled_by,settled_at)
VALUES($1,'adjustment',$2,'points',$3,$4,'succeeded',$5,$6,now())
ON CONFLICT (idempotency_key) DO NOTHING`,
					apptID, stationWeighID, adjustAmount, adjustPoints, settleKey, stationAcctID); err != nil {
					return err
				}
				// 同步补回收员服务费
				fee := math.Round(adjustAmount*0.15*100) / 100
				if _, err := t.Exec(ctx, `
INSERT INTO earnings_ledger(recycler_id,appointment_id,amount,type,idempotency_key)
VALUES($1,$2,$3,'service_fee_adjust',$4) ON CONFLICT (idempotency_key) DO NOTHING`,
					recyclerID, apptID, fee, fmt.Sprintf("earn:fee:%d:adjust:add", apptID)); err != nil {
					return err
				}
			} else {
				// 站点更轻超阈值：重量差异争议，扣减回收员服务费（不向居民追讨）
				status = "disputed"
				feeClaw := math.Round(math.Abs(adjustAmount)*0.15*100) / 100
				if _, err := t.Exec(ctx, `
INSERT INTO earnings_ledger(recycler_id,appointment_id,amount,type,idempotency_key)
VALUES($1,$2,$3,'service_fee_clawback',$4) ON CONFLICT (idempotency_key) DO NOTHING`,
					recyclerID, apptID, -feeClaw, fmt.Sprintf("earn:fee:%d:adjust:claw", apptID)); err != nil {
					return err
				}
				if _, err := t.Exec(ctx, `
INSERT INTO disputes(appointment_id,type,raised_by,title,description,status)
VALUES($1,'weight',$2,'站点复称重量明显偏轻',$3,'open')`,
					apptID, stationAcctID,
					fmt.Sprintf("现场 %.3fkg/%.2f元，站点 %.3fkg/%.2f元，差异 %.2f%%，疑似称重误差或虚高",
						onsite.TotalWeightKg, onsite.TotalAmount, totalWeight, totalAmount, diffPct)); err != nil {
					return err
				}
			}
			if totalRejected > 0 {
				if _, err := t.Exec(ctx, `
INSERT INTO disputes(appointment_id,type,raised_by,title,description,status)
VALUES($1,'contamination',$2,'入站发现不可回收物',$3,'open')
ON CONFLICT DO NOTHING`,
					apptID, stationAcctID,
					fmt.Sprintf("站点分拣出不可回收物 %.3fkg：%s", totalRejected, in.Note)); err != nil {
					return err
				}
			}
		}

		if err := t.QueryRow(ctx, `
INSERT INTO inbound_records(appointment_id,station_id,onsite_weigh_id,station_weigh_id,
       onsite_weight_kg,station_weight_kg,diff_kg,diff_pct,rejected_kg,adjustment_amount,
       adjustment_points,status,handled_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
RETURNING id`,
			apptID, stationID, onsite.ID, stationWeighID, onsite.TotalWeightKg, totalWeight,
			diffKg, diffPct, totalRejected, adjustAmount, adjustPoints, status, stationAcctID).Scan(&inboundID); err != nil {
			return err
		}

		if _, err := t.Exec(ctx,
			`UPDATE appointments SET stage='inbound', updated_at=now() WHERE id=$1`, apptID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageReweigh, "reweigh.station_weighed",
			&stationAcctID, "station", map[string]any{
				"inbound_id": inboundID, "station_weigh_id": stationWeighID,
				"onsite_weight_kg": onsite.TotalWeightKg, "station_weight_kg": totalWeight,
				"onsite_amount": onsite.TotalAmount, "station_amount": totalAmount,
				"diff_kg": diffKg, "diff_pct": diffPct, "threshold_pct": diffThresholdPct,
				"rejected_kg": totalRejected, "status": status,
				"adjust_amount": adjustAmount, "adjust_points": adjustPoints,
				"confirmed_by": stationAcctID,
				"impact": map[string]string{
					"normal":   "差异在阈值内，不调整结算，按站点重量发证",
					"adjusted": "站点更重，已以积分向居民补发差额",
					"disputed": "站点更轻超阈值，生成差异争议并扣减回收员服务费，待运营复核",
				}[status],
				"lines": lines,
			}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicInbound, bus.EventTypeInboundReweighed, map[string]any{
			"appointment_id": apptID, "inbound_id": inboundID, "station_id": stationID,
			"station_weigh_id": stationWeighID, "onsite_weight_kg": onsite.TotalWeightKg,
			"station_weight_kg": totalWeight, "diff_kg": diffKg, "diff_pct": diffPct,
			"rejected_kg": totalRejected, "status": status, "adjust_amount": adjustAmount,
			"adjust_points": adjustPoints, "resident_id": residentID,
		})
	})
	if err != nil {
		return nil, err
	}
	result["appointment_id"] = apptID
	result["station_weigh_id"] = stationWeighID
	result["inbound_id"] = inboundID
	// 凭证由 certificate worker 消费 InboundReweighed 后签发（事件驱动）。
	return result, nil
}
