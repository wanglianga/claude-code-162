package models

import (
	"time"
)

// 阶段：居民预约 / 回收员接单 / 现场定价 / 站点复称 / 积分结算 / 环保凭证
const (
	StageAppointment = "appointment"
	StageDispatch    = "dispatch"
	StageOnsite      = "onsite"
	StageReweigh     = "reweigh"
	StageSettlement  = "settlement"
	StageCertificate = "certificate"
)

type Account struct {
	ID           int64     `json:"id"`
	Role         string    `json:"role"`
	Name         string    `json:"name"`
	Phone        string    `json:"phone"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

type Resident struct {
	AccountID       int64   `json:"account_id"`
	Address         string  `json:"address"`
	Lng             float64 `json:"lng"`
	Lat             float64 `json:"lat"`
	DefaultFloor    int     `json:"default_floor"`
	DefaultElevator bool    `json:"default_elevator"`
	Elderly         bool    `json:"elderly"`
}

type Station struct {
	ID        int64   `json:"id"`
	AccountID int64   `json:"account_id"`
	Name      string  `json:"name"`
	Address   string  `json:"address"`
	Lng       float64 `json:"lng"`
	Lat       float64 `json:"lat"`
}

type Recycler struct {
	AccountID      int64   `json:"account_id"`
	StationID      int64   `json:"station_id"`
	Lng            float64 `json:"lng"`
	Lat            float64 `json:"lat"`
	ServiceRadiusM int     `json:"service_radius_m"`
	DutyStatus     string  `json:"duty_status"`
	CashFloat      float64 `json:"cash_float"`
}

type Category struct {
	Code          string  `json:"code"`
	Name          string  `json:"name"`
	UnitPrice     float64 `json:"unit_price"`
	PointsPerYuan float64 `json:"points_per_yuan"`
	CO2PerKg      float64 `json:"co2_per_kg"`
	Active        bool    `json:"active"`
	SortNo        int     `json:"sort_no"`
}

type AppointmentItem struct {
	ID           int64    `json:"id,omitempty"`
	Category     string   `json:"category"`
	EstWeightKg  float64  `json:"est_weight_kg"`
	EstUnitPrice float64  `json:"est_unit_price"`
	EstAmount    float64  `json:"est_amount"`
	PhotoURLs    []string `json:"photo_urls"`
	AddedLater   bool     `json:"added_later"`
	AddedBy      *int64   `json:"added_by,omitempty"`
}

type Appointment struct {
	ID               int64             `json:"id"`
	Code             string            `json:"code"`
	QrToken          string            `json:"qr_token"`
	ResidentID       int64             `json:"resident_id"`
	Address          string            `json:"address"`
	Lng              float64           `json:"lng"`
	Lat              float64           `json:"lat"`
	Floor            int               `json:"floor"`
	HasElevator      bool              `json:"has_elevator"`
	WindowStart      time.Time         `json:"window_start"`
	WindowEnd        time.Time         `json:"window_end"`
	PromisedArriveAt *time.Time        `json:"promised_arrive_at,omitempty"`
	EstAmount        float64           `json:"est_amount"`
	EstWeightKg      float64           `json:"est_weight_kg"`
	PriceDetail      map[string]any    `json:"price_detail"`
	Remark           string            `json:"remark,omitempty"`
	Stage            string            `json:"stage"`
	SettleMethod     *string           `json:"settle_method,omitempty"`
	Items            []AppointmentItem `json:"items,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type Dispatch struct {
	ID            int64      `json:"id"`
	AppointmentID int64      `json:"appointment_id"`
	RecyclerID    int64      `json:"recycler_id"`
	StationID     int64      `json:"station_id"`
	DistanceKm    float64    `json:"distance_km"`
	Status        string     `json:"status"`
	AssignedBy    string     `json:"assigned_by"`
	OfferedAt     time.Time  `json:"offered_at"`
	RespondedAt   *time.Time `json:"responded_at,omitempty"`
}

type WeighItem struct {
	ID             int64    `json:"id,omitempty"`
	Category       string   `json:"category"`
	WeightKg       float64  `json:"weight_kg"`
	UnitPrice      float64  `json:"unit_price"`
	Amount         float64  `json:"amount"`
	RejectedKg     float64  `json:"rejected_kg"`
	RejectedReason string   `json:"rejected_reason,omitempty"`
	PhotoURLs      []string `json:"photo_urls"`
}

type WeighRecord struct {
	ID            int64       `json:"id,omitempty"`
	AppointmentID int64       `json:"appointment_id"`
	Phase         string      `json:"phase"`
	Seq           int         `json:"seq"`
	StationID     *int64      `json:"station_id,omitempty"`
	WeighedBy     int64       `json:"weighed_by"`
	ScaleNo       string      `json:"scale_no,omitempty"`
	TotalWeightKg float64     `json:"total_weight_kg"`
	TotalAmount   float64     `json:"total_amount"`
	Status        string      `json:"status"`
	ConfirmedBy   *int64      `json:"confirmed_by,omitempty"`
	ConfirmMethod string      `json:"confirm_method,omitempty"`
	ConfirmedAt   *time.Time  `json:"confirmed_at,omitempty"`
	Note          string      `json:"note,omitempty"`
	Items         []WeighItem `json:"items"`
	CreatedAt     time.Time   `json:"created_at,omitempty"`
}

type Settlement struct {
	ID             int64      `json:"id,omitempty"`
	AppointmentID  int64      `json:"appointment_id"`
	Kind           string     `json:"kind"`
	WeighRecordID  *int64     `json:"weigh_record_id,omitempty"`
	Method         string     `json:"method"`
	Amount         float64    `json:"amount"`
	Points         int        `json:"points"`
	Status         string     `json:"status"`
	IdempotencyKey string     `json:"idempotency_key"`
	Attempts       int        `json:"attempts"`
	FailReason     string     `json:"fail_reason,omitempty"`
	SettledBy      *int64     `json:"settled_by,omitempty"`
	SettledAt      *time.Time `json:"settled_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at,omitempty"`
}

type Certificate struct {
	ID            int64            `json:"id"`
	CertNo        string           `json:"cert_no"`
	AppointmentID int64            `json:"appointment_id"`
	ResidentID    int64            `json:"resident_id"`
	StationID     int64            `json:"station_id"`
	Items         []map[string]any `json:"items"`
	TotalWeightKg float64          `json:"total_weight_kg"`
	CO2SavedKg    float64          `json:"co2_saved_kg"`
	IssuedAt      time.Time        `json:"issued_at"`
	Revoked       bool             `json:"revoked"`
}

type Dispute struct {
	ID            int64      `json:"id"`
	AppointmentID int64      `json:"appointment_id"`
	Type          string     `json:"type"`
	RaisedBy      int64      `json:"raised_by"`
	Title         string     `json:"title"`
	Description   string     `json:"description,omitempty"`
	Status        string     `json:"status"`
	Resolution    string     `json:"resolution,omitempty"`
	AdjustAmount  float64    `json:"adjust_amount"`
	AdjustPoints  int64      `json:"adjust_points"`
	ResolverID    *int64     `json:"resolver_id,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

type StageEvent struct {
	ID            int64          `json:"id"`
	AppointmentID int64          `json:"appointment_id"`
	Stage         string         `json:"stage"`
	EventType     string         `json:"event_type"`
	ActorID       *int64         `json:"actor_id,omitempty"`
	ActorRole     string         `json:"actor_role,omitempty"`
	Payload       map[string]any `json:"payload"`
	CreatedAt     time.Time      `json:"created_at"`
}
