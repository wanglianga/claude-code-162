package service

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"recycle/internal/models"
	"recycle/internal/store"
)

// testEnv 连接本地 PostgreSQL/Redis；服务未运行则跳过集成测试。
func testEnv(t *testing.T) (*Service, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "host=/workspace/162-b/.local/run port=5433 user=recycle dbname=recycling sslmode=disable"
	}
	redisAddr := os.Getenv("TEST_REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	ctx := context.Background()
	db, err := store.New(ctx, dsn)
	if err != nil {
		t.Skipf("postgres 不可用，跳过集成测试: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 15})
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		t.Skipf("redis 不可用: %v", err)
	}
	svc := New(db.Pool, rdb)
	if err := svc.Seed(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.SyncGeo(ctx); err != nil {
		t.Fatalf("geo sync: %v", err)
	}
	return svc, ctx
}

func newTestResident(t *testing.T, svc *Service, ctx context.Context, suffix string, elderly bool) int64 {
	t.Helper()
	phone := "139" + suffix
	var id int64
	err := svc.db.QueryRow(ctx, `
INSERT INTO accounts(role,name,phone,password_hash)
VALUES('resident','测试居民',$1,'x') RETURNING id`, phone).Scan(&id)
	if err != nil {
		if err2 := svc.db.QueryRow(ctx, `SELECT id FROM accounts WHERE phone=$1`, phone).Scan(&id); err2 != nil {
			t.Fatalf("create account: %v", err)
		}
		return id
	}
	if _, err := svc.db.Exec(ctx, `
INSERT INTO residents(account_id,address,lng,lat,default_floor,default_elevator,elderly)
VALUES($1,'测试地址',116.4060,39.9055,3,false,$2)
ON CONFLICT (account_id) DO NOTHING`, id, elderly); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(ctx,
		`INSERT INTO point_accounts(account_id,balance) VALUES($1,0)
		 ON CONFLICT DO NOTHING`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFullLifecyclePointsAndCertificate(t *testing.T) {
	svc, ctx := testEnv(t)
	rid := newTestResident(t, svc, ctx, fmt.Sprintf("00%03d", time.Now().UnixMicro()%1000), false)
	recv := int64(0)
	if err := svc.db.QueryRow(ctx, `SELECT account_id FROM recyclers WHERE duty_status='on' ORDER BY account_id LIMIT 1`).Scan(&recv); err != nil {
		t.Fatal(err)
	}

	a, err := svc.CreateAppointment(ctx, rid, CreateAppointmentInput{
		Items:       []ItemInput{{Category: "paper", WeightKg: 10}},
		WindowStart: time.Now().Add(time.Hour),
		WindowEnd:   time.Now().Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create appointment: %v", err)
	}
	if a.EstAmount <= 0 {
		t.Fatalf("est amount should be positive: %v", a.EstAmount)
	}
	if err := svc.AcceptDispatch(ctx, recv, a.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if err := svc.Enroute(ctx, recv, a.ID); err != nil {
		t.Fatalf("enroute: %v", err)
	}
	if _, err := svc.Arrival(ctx, recv, a.ID); err != nil {
		t.Fatalf("arrive: %v", err)
	}
	if err := svc.VerifyResident(ctx, recv, a.ID, VerifyInput{QRToken: a.QrToken, Method: "qr_scan"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, err := svc.OnsiteWeigh(ctx, recv, a.ID, WeighInput{
		ScaleNo: "T-1", Items: []WeighItemInput{{Category: "paper", WeightKg: 9.8}},
	}); err != nil {
		t.Fatalf("weigh: %v", err)
	}
	if err := svc.ConfirmWeigh(ctx, rid, a.ID, true, "self", ""); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	st, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "points"})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if st.Status != "succeeded" || st.Points <= 0 {
		t.Fatalf("settlement bad: %+v", st)
	}
	bal, _ := svc.PointBalance(ctx, rid)
	if bal != int64(st.Points) {
		t.Fatalf("balance=%d points=%d", bal, st.Points)
	}
	// 重复结算必须幂等
	st2, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "points"})
	if err != nil || st2.ID != st.ID {
		t.Fatalf("idempotent re-settle mismatch: %+v err=%v", st2, err)
	}
	bal2, _ := svc.PointBalance(ctx, rid)
	if bal2 != bal {
		t.Fatalf("重复结算导致积分重复到账: %d -> %d", bal, bal2)
	}

	// 站点复称（±5% 内）
	if _, err := svc.InboundWeigh(ctx, stationAcct(t, svc, ctx), a.ID, WeighInput{
		ScaleNo: "ST-T", Items: []WeighItemInput{{Category: "paper", WeightKg: 9.9}},
	}); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	cert, err := svc.IssueCertificate(ctx, a.ID)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	if cert.TotalWeightKg != 9.9 || cert.CertNo == "" {
		t.Fatalf("certificate bad: %+v", cert)
	}
	// 凭证重复签发必须幂等
	cert2, err := svc.IssueCertificate(ctx, a.ID)
	if err != nil || cert2.ID != cert.ID {
		t.Fatalf("certificate not idempotent: %+v", cert2)
	}

	// 时间线必须覆盖六环节
	tl, err := svc.Timeline(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range tl {
		seen[e.Stage] = true
	}
	for _, stg := range []string{models.StageAppointment, models.StageDispatch, models.StageOnsite,
		models.StageReweigh, models.StageSettlement, models.StageCertificate} {
		if !seen[stg] {
			t.Fatalf("时间线缺少环节 %s: %v", stg, seen)
		}
	}
}

func TestInboundHeavierAddsAdjustmentPoints(t *testing.T) {
	svc, ctx := testEnv(t)
	rid := newTestResident(t, svc, ctx, fmt.Sprintf("01%03d", time.Now().UnixMicro()%1000), false)
	var recv int64
	if err := svc.db.QueryRow(ctx, `SELECT account_id FROM recyclers WHERE duty_status='on' ORDER BY account_id LIMIT 1`).Scan(&recv); err != nil {
		t.Fatal(err)
	}
	a, err := svc.CreateAppointment(ctx, rid, CreateAppointmentInput{
		Items:       []ItemInput{{Category: "metal", WeightKg: 8}},
		WindowStart: time.Now().Add(time.Hour), WindowEnd: time.Now().Add(2 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = svc.AcceptDispatch(ctx, recv, a.ID)
	_ = svc.Enroute(ctx, recv, a.ID)
	_, _ = svc.Arrival(ctx, recv, a.ID)
	if err := svc.VerifyResident(ctx, recv, a.ID, VerifyInput{QRToken: a.QrToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnsiteWeigh(ctx, recv, a.ID, WeighInput{
		Items: []WeighItemInput{{Category: "metal", WeightKg: 8.0}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmWeigh(ctx, rid, a.ID, true, "self", ""); err != nil {
		t.Fatal(err)
	}
	st, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "points"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := svc.PointBalance(ctx, rid)
	// 站点复称重 10%
	if _, err := svc.InboundWeigh(ctx, stationAcct(t, svc, ctx), a.ID, WeighInput{
		Items: []WeighItemInput{{Category: "metal", WeightKg: 8.8}},
	}); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	after, _ := svc.PointBalance(ctx, rid)
	if after <= before {
		t.Fatalf("复称更重应补发积分: %d -> %d", before, after)
	}
	_ = st
}

func TestDisputedWeighBlocksSettlement(t *testing.T) {
	svc, ctx := testEnv(t)
	rid := newTestResident(t, svc, ctx, fmt.Sprintf("02%03d", time.Now().UnixMicro()%1000), false)
	var recv int64
	if err := svc.db.QueryRow(ctx, `SELECT account_id FROM recyclers WHERE duty_status='on' ORDER BY account_id LIMIT 1`).Scan(&recv); err != nil {
		t.Fatal(err)
	}
	a, _ := svc.CreateAppointment(ctx, rid, CreateAppointmentInput{
		Items:       []ItemInput{{Category: "plastic", WeightKg: 5}},
		WindowStart: time.Now().Add(time.Hour), WindowEnd: time.Now().Add(2 * time.Hour),
	})
	_ = svc.AcceptDispatch(ctx, recv, a.ID)
	_, _ = svc.Arrival(ctx, recv, a.ID)
	if err := svc.VerifyResident(ctx, recv, a.ID, VerifyInput{QRToken: a.QrToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OnsiteWeigh(ctx, recv, a.ID, WeighInput{
		Items: []WeighItemInput{{Category: "plastic", WeightKg: 4.0}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmWeigh(ctx, rid, a.ID, false, "self", "重量不对"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "cash"}); err == nil {
		t.Fatal("争议状态下不应允许结算")
	}
	// 重称 4.6、重新确认后可结算
	if _, err := svc.OnsiteWeigh(ctx, recv, a.ID, WeighInput{
		Items: []WeighItemInput{{Category: "plastic", WeightKg: 4.6}},
	}); err != nil {
		t.Fatalf("re-weigh: %v", err)
	}
	if err := svc.ConfirmWeigh(ctx, rid, a.ID, true, "self", ""); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}
	st, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "cash"})
	if err != nil || st.Status != "succeeded" {
		t.Fatalf("重称确认后现金结算应成功: %+v %v", st, err)
	}
}

func TestCashFailureThenPointsSuccess(t *testing.T) {
	svc, ctx := testEnv(t)
	rid := newTestResident(t, svc, ctx, fmt.Sprintf("03%03d", time.Now().UnixMicro()%1000), false)
	var recv int64
	if err := svc.db.QueryRow(ctx, `SELECT account_id FROM recyclers WHERE duty_status='on' ORDER BY account_id LIMIT 1`).Scan(&recv); err != nil {
		t.Fatal(err)
	}
	a, _ := svc.CreateAppointment(ctx, rid, CreateAppointmentInput{
		Items:       []ItemInput{{Category: "textile", WeightKg: 3}},
		WindowStart: time.Now().Add(time.Hour), WindowEnd: time.Now().Add(2 * time.Hour),
	})
	_ = svc.AcceptDispatch(ctx, recv, a.ID)
	_, _ = svc.Arrival(ctx, recv, a.ID)
	_ = svc.VerifyResident(ctx, recv, a.ID, VerifyInput{QRToken: a.QrToken})
	_, _ = svc.OnsiteWeigh(ctx, recv, a.ID, WeighInput{
		Items: []WeighItemInput{{Category: "textile", WeightKg: 3.0}},
	})
	if err := svc.AssistConfirmWeigh(ctx, recv, a.ID, "当面征得同意后代确认"); err != nil {
		t.Fatalf("assist confirm: %v", err)
	}
	st, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "cash", SimulateFail: true})
	if err != nil {
		t.Fatalf("失败结算不应返回错误而应落 failed 记录: %v", err)
	}
	if st.Status != "failed" || st.FailReason == "" {
		t.Fatalf("应记录失败原因: %+v", st)
	}
	st2, err := svc.Settle(ctx, rid, a.ID, SettleInput{Method: "points"})
	if err != nil || st2.Status != "succeeded" {
		t.Fatalf("改积分重试应成功: %+v %v", st2, err)
	}
	if st2.Attempts < 1 {
		t.Fatalf("attempts 应记录重试次数: %+v", st2)
	}
}

func stationAcct(t *testing.T, svc *Service, ctx context.Context) int64 {
	t.Helper()
	var id int64
	if err := svc.db.QueryRow(ctx, `SELECT account_id FROM stations LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
