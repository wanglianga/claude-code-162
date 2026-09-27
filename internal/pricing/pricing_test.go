package pricing

import (
	"math"
	"testing"
)

func TestEstimateFormula(t *testing.T) {
	// 一层、2km 内：仅基础价（单价×重量×0.95）
	d := Estimate(Input{
		Lines:      []Line{{Category: "paper", WeightKg: 10, Price: 1.2}},
		Floor:      1,
		DistanceKm: 1,
	})
	if math.Abs(d.BaseAmount-11.4) > 0.001 {
		t.Fatalf("base amount = %v, want 11.40", d.BaseAmount)
	}
	if d.FloorSurcharge != 0 || d.DistanceAdjust != 0 {
		t.Fatalf("expect no surcharges, got floor=%v dist=%v", d.FloorSurcharge, d.DistanceAdjust)
	}
	if d.EstAmount != 11.4 {
		t.Fatalf("est amount = %v, want 11.4", d.EstAmount)
	}
}

func TestEstimateFloorAndDistance(t *testing.T) {
	// 6 层无电梯、20kg：楼层附加 min(20, (6-1)*0.05*20)=5；6km 折让 6%
	d := Estimate(Input{
		Lines:       []Line{{Category: "paper", WeightKg: 20, Price: 1}},
		Floor:       6,
		HasElevator: false,
		DistanceKm:  6,
	})
	if d.FloorSurcharge != 5 {
		t.Fatalf("floor surcharge = %v, want 5", d.FloorSurcharge)
	}
	base := 19.0 // 20*1*0.95
	if math.Abs(d.DistanceAdjust+base*0.06) > 0.001 {
		t.Fatalf("distance adjust = %v, want %v", d.DistanceAdjust, -base*0.06)
	}
	want := round2(base + 5 - base*0.06)
	if d.EstAmount != want {
		t.Fatalf("est = %v, want %v", d.EstAmount, want)
	}
	// 有电梯同楼层无附加
	d2 := Estimate(Input{
		Lines: []Line{{Category: "paper", WeightKg: 20, Price: 1}},
		Floor: 6, HasElevator: true, DistanceKm: 1,
	})
	if d2.FloorSurcharge != 0 {
		t.Fatalf("elevator floor surcharge = %v, want 0", d2.FloorSurcharge)
	}
}

func TestHaversine(t *testing.T) {
	// 同点 0；约 111km/度（赤道附近）
	if d := HaversineKm(0, 0, 0, 0); d != 0 {
		t.Fatalf("same point distance = %v", d)
	}
	d := HaversineKm(116.4050, 39.9050, 116.4100, 39.9070)
	if d <= 0 || d > 1 {
		t.Fatalf("seed points should be <1km apart, got %v", d)
	}
}
