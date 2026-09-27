package pricing

import (
	"math"
)

// Line 估价品类行
type Line struct {
	Category string  `json:"category"`
	WeightKg float64 `json:"weight_kg"`
	Price    float64 `json:"price"`
}

// Input 估价输入
type Input struct {
	Lines       []Line  `json:"lines"`
	Floor       int     `json:"floor"`
	HasElevator bool    `json:"has_elevator"`
	DistanceKm  float64 `json:"distance_km"`
}

// Detail 估价明细（价格透明：每一项加减都可解释）
type Detail struct {
	BaseAmount     float64      `json:"base_amount"`
	FloorSurcharge float64      `json:"floor_surcharge"`
	FloorReason    string       `json:"floor_reason"`
	DistanceAdjust float64      `json:"distance_adjust"`
	DistanceReason string       `json:"distance_reason"`
	TotalWeightKg  float64      `json:"total_weight_kg"`
	EstAmount      float64      `json:"est_amount"`
	EstPoints      int64        `json:"est_points"`
	Lines          []LineDetail `json:"lines"`
	Formula        string       `json:"formula"`
}

type LineDetail struct {
	Category string  `json:"category"`
	WeightKg float64 `json:"weight_kg"`
	Price    float64 `json:"price"`
	Amount   float64 `json:"amount"`
}

// 楼层搬运附加（无电梯按楼层补贴，回收成本项，体现到估价）
func floorSurcharge(floor int, hasElevator bool, weight float64) (float64, string) {
	if floor <= 1 || hasElevator {
		return 0, "一层或有电梯，无楼层附加"
	}
	// 每高一层、每公斤 0.05 元，封顶 20 元
	s := math.Min(20, float64(floor-1)*0.05*weight)
	return round2(s), "无电梯步行楼层搬运附加"
}

// 距离调整：2km 内不调整；2-5km 减 3%；5km 以上减 6%（远距离运输折让，估价更保守）
func distanceAdjust(km float64, base float64) (float64, string) {
	switch {
	case km <= 2:
		return 0, "距离 2km 以内，无折让"
	case km <= 5:
		return -round2(base * 0.03), "距离 2-5km，运输折让 3%"
	default:
		return -round2(base * 0.06), "距离 5km 以上，运输折让 6%"
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

// Estimate 预估价：市场单价 × 预计重量（居民自报通常上浮，统一 ×0.95 保守系数），
// 叠加楼层附加与距离折让。积分按 1 元 100 分估算（实际品类系数在结算时计算）。
func Estimate(in Input) Detail {
	d := Detail{Formula: "sum(单价×重量×0.95保守系数) + 楼层附加 + 距离折让"}
	var base float64
	for _, l := range in.Lines {
		w := round3(l.WeightKg)
		amount := round2(l.Price * w * 0.95)
		base += amount
		d.TotalWeightKg = round3(d.TotalWeightKg + w)
		d.Lines = append(d.Lines, LineDetail{
			Category: l.Category, WeightKg: w, Price: l.Price, Amount: amount,
		})
	}
	base = round2(base)
	d.BaseAmount = base

	d.FloorSurcharge, d.FloorReason = floorSurcharge(in.Floor, in.HasElevator, d.TotalWeightKg)
	d.DistanceAdjust, d.DistanceReason = distanceAdjust(in.DistanceKm, base)
	d.EstAmount = round2(base + d.FloorSurcharge + d.DistanceAdjust)
	if d.EstAmount < 0 {
		d.EstAmount = 0
	}
	d.EstPoints = int64(math.Round(d.EstAmount * 100))
	return d
}

// HaversineKm 两点球面距离（公里）。
func HaversineKm(lng1, lat1, lng2, lat2 float64) float64 {
	const r = 6371.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return r * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// SettleAmount 现场/站点实际结算金额：按实测重量与现场单价，无保守系数，无折让。
func SettleAmount(weightKg, unitPrice float64) float64 {
	return round2(round3(weightKg) * unitPrice)
}
