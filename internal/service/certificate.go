package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"recycle/internal/bus"
	"recycle/internal/models"
)

// IssueCertificate 入站复称后按站点实收重量与品类开具环保凭证（幂等）。
// 由凭证 worker 调用，也可由站点/运营手工触发补开。
func (s *Service) IssueCertificate(ctx context.Context, apptID int64) (*models.Certificate, error) {
	a, err := s.getAppointment(ctx, apptID)
	if err != nil {
		return nil, err
	}
	if a.Stage != "inbound" && a.Stage != "certified" && a.Stage != "closed" {
		return nil, bizError("STAGE_CONFLICT", "需完成站点复称后才能开具凭证，当前环节: "+a.Stage)
	}
	// 已签发则直接返回（幂等）
	var existingID int64
	err = s.db.QueryRow(ctx, `SELECT id FROM certificates WHERE appointment_id=$1 AND revoked=false`, apptID).Scan(&existingID)
	if err == nil {
		return s.getCertificate(ctx, existingID)
	}
	if err != pgx.ErrNoRows {
		return nil, err
	}

	var stationWeighID, stationID, residentID int64
	if err := s.db.QueryRow(ctx, `
SELECT i.station_weigh_id, i.station_id, a.resident_id
FROM inbound_records i JOIN appointments a ON a.id=i.appointment_id
WHERE i.appointment_id=$1`, apptID).Scan(&stationWeighID, &stationID, &residentID); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(ctx, `
SELECT wi.category, c.name, wi.weight_kg - wi.rejected_kg, wi.amount, c.co2_per_kg
FROM weigh_items wi JOIN categories c ON c.code=wi.category
WHERE wi.weigh_record_id=$1 ORDER BY wi.id`, stationWeighID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type cLine struct {
		category, name string
		weight, amount float64
		co2            float64
	}
	var lines []cLine
	var totalWeight, totalCO2 float64
	for rows.Next() {
		var l cLine
		if err := rows.Scan(&l.category, &l.name, &l.weight, &l.amount, &l.co2); err != nil {
			return nil, err
		}
		lines = append(lines, l)
		totalWeight += l.weight
		totalCO2 += l.weight * l.co2
	}
	totalWeight = math.Round(totalWeight*1000) / 1000
	totalCO2 = math.Round(totalCO2*1000) / 1000

	certNo := fmt.Sprintf("CER%s%06d", a.CreatedAt.Format("20060102"), apptID)
	itemPayload := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		itemPayload = append(itemPayload, map[string]any{
			"category": l.category, "name": l.name,
			"weight_kg": math.Round(l.weight*1000) / 1000, "amount": l.amount,
			"co2_saved_kg": math.Round(l.weight*l.co2*1000) / 1000,
		})
	}

	var certID int64
	itemsJSON, err := json.Marshal(itemPayload)
	if err != nil {
		return nil, err
	}
	err = s.tx(ctx, func(t pgx.Tx) error {
		if err := t.QueryRow(ctx, `
INSERT INTO certificates(cert_no,appointment_id,resident_id,station_id,items,total_weight_kg,co2_saved_kg)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			certNo, apptID, residentID, stationID, itemsJSON, totalWeight, totalCO2).Scan(&certID); err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`UPDATE appointments SET stage='certified', updated_at=now() WHERE id=$1`, apptID); err != nil {
			return err
		}
		if err := addEvent(ctx, t, apptID, models.StageCertificate, "certificate.issued", nil, "system", map[string]any{
			"certificate_id": certID, "cert_no": certNo,
			"total_weight_kg": totalWeight, "co2_saved_kg": totalCO2,
			"items":  itemPayload,
			"impact": "凭证以站点复称实收品类与重量为准，是六环节的最终凭据",
		}); err != nil {
			return err
		}
		return addOutbox(ctx, t, apptRef(apptID), bus.TopicCertificate, bus.EventTypeCertificateIssued, map[string]any{
			"appointment_id": apptID, "certificate_id": certID, "cert_no": certNo,
			"resident_id": residentID, "station_id": stationID,
			"total_weight_kg": totalWeight, "co2_saved_kg": totalCO2, "items": itemPayload,
		})
	})
	if err != nil {
		return nil, err
	}
	return s.getCertificate(ctx, certID)
}

func (s *Service) getCertificate(ctx context.Context, id int64) (*models.Certificate, error) {
	var c models.Certificate
	var items []byte
	err := s.db.QueryRow(ctx, `
SELECT id,cert_no,appointment_id,resident_id,station_id,items,total_weight_kg,co2_saved_kg,issued_at,revoked
FROM certificates WHERE id=$1`, id).Scan(&c.ID, &c.CertNo, &c.AppointmentID, &c.ResidentID,
		&c.StationID, &items, &c.TotalWeightKg, &c.CO2SavedKg, &c.IssuedAt, &c.Revoked)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(items, &c.Items)
	return &c, nil
}

// GetCertificateByAppointment 查询某预约的凭证。
func (s *Service) GetCertificateByAppointment(ctx context.Context, apptID int64) (*models.Certificate, error) {
	var id int64
	err := s.db.QueryRow(ctx, `SELECT id FROM certificates WHERE appointment_id=$1`, apptID).Scan(&id)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.getCertificate(ctx, id)
}
