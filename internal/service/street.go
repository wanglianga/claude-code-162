package service

import (
	"context"
	"math"

	"github.com/jackc/pgx/v5"

	"recycle/internal/models"
)

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// StreetDashboard 街道视角：参与人数、品类重量/金额、资源流向（站点分布）。
func (s *Service) StreetDashboard(ctx context.Context) (map[string]any, error) {
	// 按品类汇总（以站点复称后的实收重量为准，凭证口径一致）
	rows, err := s.db.Query(ctx, `
SELECT wi.category, c.name,
       COUNT(DISTINCT cert.appointment_id),
       COALESCE(SUM(wi.weight_kg - wi.rejected_kg),0),
       COALESCE(SUM(wi.amount),0),
       COALESCE(SUM((wi.weight_kg - wi.rejected_kg)*c.co2_per_kg),0)
FROM certificates cert
JOIN inbound_records ir ON ir.appointment_id = cert.appointment_id
JOIN weigh_records w ON w.id = ir.station_weigh_id
JOIN weigh_items wi ON wi.weigh_record_id = w.id
JOIN categories c ON c.code = wi.category
WHERE cert.revoked = false
GROUP BY wi.category, c.name
ORDER BY SUM(wi.weight_kg - wi.rejected_kg) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byCategory := []map[string]any{}
	var totalWeight, totalAmount, totalCO2 float64
	for rows.Next() {
		var code, name string
		var cnt int64
		var weight, amount, co2 float64
		if err := rows.Scan(&code, &name, &cnt, &weight, &amount, &co2); err != nil {
			return nil, err
		}
		byCategory = append(byCategory, map[string]any{
			"category": code, "name": name, "certificate_count": cnt,
			"weight_kg": weight, "amount": amount, "co2_saved_kg": co2,
		})
		totalWeight += weight
		totalAmount += amount
		totalCO2 += co2
	}

	// 凭证总数 / 参与居民数（按凭证去重，不能按品类行求和）
	var totalOrders, participants int64
	if err := s.db.QueryRow(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT resident_id) FROM certificates WHERE revoked=false`).
		Scan(&totalOrders, &participants); err != nil {
		return nil, err
	}

	// 资源流向：每个站点接收的重量与品类数
	srows, err := s.db.Query(ctx, `
SELECT st.id, st.name,
       COUNT(DISTINCT cert.appointment_id),
       COALESCE(SUM(wi.weight_kg - wi.rejected_kg),0)
FROM certificates cert
JOIN stations st ON st.id = cert.station_id
JOIN inbound_records ir ON ir.appointment_id = cert.appointment_id
JOIN weigh_records w ON w.id = ir.station_weigh_id
JOIN weigh_items wi ON wi.weigh_record_id = w.id
WHERE cert.revoked = false
GROUP BY st.id, st.name
ORDER BY SUM(wi.weight_kg - wi.rejected_kg) DESC`)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	flow := []map[string]any{}
	for srows.Next() {
		var id, cnt int64
		var name string
		var weight float64
		if err := srows.Scan(&id, &name, &cnt, &weight); err != nil {
			return nil, err
		}
		flow = append(flow, map[string]any{
			"station_id": id, "station_name": name,
			"certificate_count": cnt, "weight_kg": weight,
		})
	}

	// 各环节在途单数（运营健康度）
	stageRows, err := s.db.Query(ctx, `
SELECT stage, COUNT(*) FROM appointments
WHERE stage NOT IN ('canceled','closed') GROUP BY stage ORDER BY stage`)
	if err != nil {
		return nil, err
	}
	defer stageRows.Close()
	pipeline := map[string]int64{}
	for stageRows.Next() {
		var st string
		var cnt int64
		if err := stageRows.Scan(&st, &cnt); err != nil {
			return nil, err
		}
		pipeline[st] = cnt
	}

	// 待处理争议/投诉
	var openDisputes, openComplaints int64
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM disputes WHERE status IN ('open','reviewing')`).Scan(&openDisputes)
	_ = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM complaints WHERE status<>'closed'`).Scan(&openComplaints)

	return map[string]any{
		"total_certificates": totalOrders,
		"participants":       participants,
		"total_weight_kg":    round3(totalWeight),
		"total_amount":       round2(totalAmount),
		"total_co2_saved_kg": round3(totalCO2),
		"by_category":        byCategory,
		"resource_flow":      flow,
		"pipeline":           pipeline,
		"open_disputes":      openDisputes,
		"open_complaints":    openComplaints,
	}, nil
}

// ListCategories 当前品类与市价（价格透明）。
func (s *Service) ListCategories(ctx context.Context) ([]models.Category, error) {
	rows, err := s.db.Query(ctx, `
SELECT code,name,unit_price,points_per_yuan,co2_per_kg,active,sort_no
FROM categories ORDER BY sort_no`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Category
	for rows.Next() {
		var c models.Category
		if err := rows.Scan(&c.Code, &c.Name, &c.UnitPrice, &c.PointsPerYuan,
			&c.CO2PerKg, &c.Active, &c.SortNo); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// UpdatePrice 运营调整市价并保留历史。
func (s *Service) UpdatePrice(ctx context.Context, code string, price float64, note string) error {
	if price <= 0 {
		return bizError("PRICE_INVALID", "单价必须大于 0")
	}
	return s.tx(ctx, func(t pgx.Tx) error {
		ct, err := t.Exec(ctx, `UPDATE categories SET unit_price=$2 WHERE code=$1 AND active=true`, code, price)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return bizError("CATEGORY_UNKNOWN", "品类不存在或已停收")
		}
		_, err = t.Exec(ctx,
			`INSERT INTO category_price_history(code,unit_price,note) VALUES($1,$2,$3)`,
			code, price, note)
		return err
	})
}

// SetDuty 回收员上/下班；上线时同步位置到 Redis GEO，下线时移除。
func (s *Service) SetDuty(ctx context.Context, recyclerID int64, on bool, lng, lat float64) error {
	if err := s.tx(ctx, func(t pgx.Tx) error {
		if on {
			if _, err := t.Exec(ctx,
				`UPDATE recyclers SET duty_status='on', lng=$2, lat=$3 WHERE account_id=$1`,
				recyclerID, lng, lat); err != nil {
				return err
			}
		} else {
			if _, err := t.Exec(ctx,
				`UPDATE recyclers SET duty_status='off' WHERE account_id=$1`, recyclerID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if on {
		return s.disp.SetLocation(ctx, recyclerID, lng, lat)
	}
	return s.disp.RemoveLocation(ctx, recyclerID)
}

// SyncGeo 把在岗回收员位置同步到 Redis（启动与上/下班时调用）。
func (s *Service) SyncGeo(ctx context.Context) error {
	rows, err := s.db.Query(ctx,
		`SELECT account_id,lng,lat FROM recyclers WHERE duty_status='on'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var lng, lat float64
		if err := rows.Scan(&id, &lng, &lat); err != nil {
			return err
		}
		if err := s.disp.SetLocation(ctx, id, lng, lat); err != nil {
			return err
		}
	}
	return nil
}
