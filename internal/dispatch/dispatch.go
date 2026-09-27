package dispatch

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const geoKey = "recyclers:geo"

// Dispatcher 基于 Redis GEO 的附近回收员派单。
type Dispatcher struct {
	rdb *redis.Client
}

func New(rdb *redis.Client) *Dispatcher {
	return &Dispatcher{rdb: rdb}
}

// SetLocation 回收员上线/更新位置。
func (d *Dispatcher) SetLocation(ctx context.Context, recyclerID int64, lng, lat float64) error {
	return d.rdb.GeoAdd(ctx, geoKey, &redis.GeoLocation{
		Name:      fmt.Sprintf("%d", recyclerID),
		Longitude: lng,
		Latitude:  lat,
	}).Err()
}

func (d *Dispatcher) RemoveLocation(ctx context.Context, recyclerID int64) error {
	return d.rdb.ZRem(ctx, geoKey, fmt.Sprintf("%d", recyclerID)).Err()
}

// Candidate 候选回收员
type Candidate struct {
	RecyclerID int64
	DistanceKm float64
}

// Nearby 查找半径 radiusKm 内、当前在线的回收员，按距离升序。
// onDuty 为 DB 判定后处于排班+在岗状态的回收员集合。
func (d *Dispatcher) Nearby(ctx context.Context, lng, lat float64, radiusKm float64, onDuty map[int64]bool) ([]Candidate, error) {
	res, err := d.rdb.GeoSearchLocation(ctx, geoKey, &redis.GeoSearchLocationQuery{
		GeoSearchQuery: redis.GeoSearchQuery{
			Longitude:  lng,
			Latitude:   lat,
			Radius:     radiusKm,
			RadiusUnit: "km",
			Sort:       "ASC",
			Count:      20,
		},
		WithDist: true,
	}).Result()
	if err != nil {
		return nil, err
	}
	var out []Candidate
	for _, loc := range res {
		var id int64
		if _, err := fmt.Sscanf(loc.Name, "%d", &id); err != nil {
			continue
		}
		if len(onDuty) > 0 && !onDuty[id] {
			continue
		}
		out = append(out, Candidate{RecyclerID: id, DistanceKm: loc.Dist})
	}
	return out, nil
}

// OfferLock 防止同一回收员短时间被重复推送（可选保护）。
func (d *Dispatcher) OfferLock(ctx context.Context, apptID int64, recyclerID int64, ttl time.Duration) (bool, error) {
	key := fmt.Sprintf("offer:lock:%d:%d", apptID, recyclerID)
	return d.rdb.SetNX(ctx, key, "1", ttl).Result()
}
