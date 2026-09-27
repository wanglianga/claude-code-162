package service

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Seed 初始化演示数据（幂等）：5 类账号、2 名回收员、1 个站点、3 位居民、
// 5 个品类与当日排班。测试账号统一密码 123456。
func (s *Service) Seed(ctx context.Context) error {
	return s.tx(ctx, func(t pgx.Tx) error {
		exists := false
		if err := t.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE phone='13900000000')`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}

		mk := func(role, name, phone string) (int64, error) {
			return s.createAccount(ctx, t, role, name, phone, "123456")
		}

		// 街道与运营
		streetID, err := mk("street", "和平街道办", "13900000000")
		if err != nil {
			return err
		}
		if _, err := mk("operator", "平台运营员", "13700000000"); err != nil {
			return err
		}

		// 回收站
		stAcct, err := mk("station", "阳光回收站", "13800000001")
		if err != nil {
			return err
		}
		var stationID int64
		if err := t.QueryRow(ctx,
			`INSERT INTO stations(account_id,name,address,lng,lat)
			 VALUES($1,'阳光回收站','和平街 8 号院',116.4050,39.9050) RETURNING id`,
			stAcct).Scan(&stationID); err != nil {
			return err
		}

		// 回收员（驻点在站点附近，默认在岗在线）
		r1, err := mk("recycler", "王强（回收员）", "13800000011")
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`INSERT INTO recyclers(account_id,station_id,lng,lat,service_radius_m,duty_status,cash_float)
			 VALUES($1,$2,116.4052,39.9052,5000,'on',500)`, r1, stationID); err != nil {
			return err
		}
		r2, err := mk("recycler", "李敏（回收员）", "13800000012")
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx,
			`INSERT INTO recyclers(account_id,station_id,lng,lat,service_radius_m,duty_status,cash_float)
			 VALUES($1,$2,116.4080,39.9090,5000,'on',500)`, r2, stationID); err != nil {
			return err
		}

		// 居民：张奶奶（老人，无电梯 6 层）、陈先生（电梯 12 层）、刘阿姨（一层）
		people := []struct {
			name, phone, addr string
			lng, lat          float64
			floor             int
			elevator, elderly bool
		}{
			{"张奶奶", "13800000021", "和平街 3 号楼 3 单元", 116.4065, 39.9058, 6, false, true},
			{"陈先生", "13800000022", "和平街 12 号院 2 号楼", 116.4100, 39.9070, 12, true, false},
			{"刘阿姨", "13800000023", "和平街 5 号院平房", 116.4030, 39.9030, 1, false, false},
		}
		for _, p := range people {
			id, err := mk("resident", p.name, p.phone)
			if err != nil {
				return err
			}
			if _, err := t.Exec(ctx,
				`INSERT INTO residents(account_id,address,lng,lat,default_floor,default_elevator,elderly)
				 VALUES($1,$2,$3,$4,$5,$6,$7)`,
				id, p.addr, p.lng, p.lat, p.floor, p.elevator, p.elderly); err != nil {
				return err
			}
			if _, err := t.Exec(ctx,
				`INSERT INTO point_accounts(account_id,balance) VALUES($1,0)`, id); err != nil {
				return err
			}
		}

		// 品类与市价、积分系数、减排因子（kg CO2 / kg）
		cats := []struct {
			code, name      string
			price, ppY, co2 float64
			sortNo          int
		}{
			{"paper", "废纸", 1.20, 100, 1.30, 1},
			{"plastic", "塑料", 0.80, 100, 1.50, 2},
			{"metal", "金属", 2.50, 100, 2.00, 3},
			{"appliance", "小家电", 3.00, 150, 1.80, 4},
			{"textile", "旧衣物", 0.50, 120, 3.60, 5},
		}
		for _, c := range cats {
			if _, err := t.Exec(ctx,
				`INSERT INTO categories(code,name,unit_price,points_per_yuan,co2_per_kg,sort_no)
				 VALUES($1,$2,$3,$4,$5,$6)
				 ON CONFLICT (code) DO NOTHING`,
				c.code, c.name, c.price, c.ppY, c.co2, c.sortNo); err != nil {
				return err
			}
			if _, err := t.Exec(ctx,
				`INSERT INTO category_price_history(code,unit_price,note) VALUES($1,$2,'seed')`,
				c.code, c.price); err != nil {
				return err
			}
		}

		// 今日排班：08:00 - 20:00
		for _, rid := range []int64{r1, r2} {
			if _, err := t.Exec(ctx,
				`INSERT INTO recycler_schedules(recycler_id,work_date,start_min,end_min)
				 VALUES($1, CURRENT_DATE, 480, 1200) ON CONFLICT DO NOTHING`, rid); err != nil {
				return err
			}
		}

		_ = streetID
		return nil
	})
}
