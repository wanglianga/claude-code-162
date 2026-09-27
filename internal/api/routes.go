package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"recycle/internal/auth"
	"recycle/internal/models"
	"recycle/internal/service"
)

func (s *Server) routes() {
	mw := func(role string, h http.HandlerFunc) http.Handler {
		return auth.Middleware(s.secret, role)(h)
	}
	anyMW := func(h http.HandlerFunc) http.Handler {
		return auth.Middleware(s.secret)(h)
	}

	s.mux.HandleFunc("/api/login", s.login)
	s.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	s.mux.Handle("/api/appointments", mw("resident", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		switch r.Method {
		case http.MethodPost:
			var in service.CreateAppointmentInput
			if !decode(w, r, &in) {
				return
			}
			a, err := s.svc.CreateAppointment(r.Context(), c.Sub, in)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, 201, a)
		case http.MethodGet:
			stage := r.URL.Query().Get("stage")
			list, err := s.svc.ListAppointments(r.Context(), c.Sub, stage, 0)
			if err != nil {
				writeErr(w, err)
				return
			}
			writeJSON(w, 200, list)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	s.mux.Handle("/api/appointments/", anyMW(s.appointmentResource))

	s.mux.Handle("/api/recycler/dispatch", mw("recycler", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		list, err := s.svc.ListDispatches(r.Context(), c.Sub, r.URL.Query().Get("status"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, list)
	}))
	s.mux.Handle("/api/recycler/earnings", mw("recycler", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		res, err := s.svc.RecyclerEarnings(r.Context(), c.Sub)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	}))
	s.mux.Handle("/api/recycler/duty", mw("recycler", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		c := claims(r)
		var in struct {
			On  bool    `json:"on"`
			Lng float64 `json:"lng"`
			Lat float64 `json:"lat"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Lng == 0 {
			in.Lng = 116.4052
			in.Lat = 39.9052
		}
		if err := s.svc.SetDuty(r.Context(), c.Sub, in.On, in.Lng, in.Lat); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"duty": in.On})
	}))

	s.mux.Handle("/api/points/balance", mw("resident", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		b, err := s.svc.PointBalance(r.Context(), c.Sub)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]int64{"balance": b})
	}))
	s.mux.Handle("/api/points/ledger", mw("resident", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		list, err := s.svc.PointLedger(r.Context(), c.Sub, limit)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, list)
	}))

	s.mux.Handle("/api/categories", anyMW(func(w http.ResponseWriter, r *http.Request) {
		list, err := s.svc.ListCategories(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, list)
	}))
	s.mux.Handle("/api/categories/", mw("operator", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/categories/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[1] != "price" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Price float64 `json:"unit_price"`
			Note  string  `json:"note"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.svc.UpdatePrice(r.Context(), parts[0], in.Price, in.Note); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "updated"})
	}))

	s.mux.Handle("/api/disputes", anyMW(func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if c.Role != "operator" && c.Role != "street" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "仅运营/街道可查看全部争议"})
			return
		}
		list, err := s.svc.ListDisputes(r.Context(), r.URL.Query().Get("status"))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, list)
	}))
	s.mux.Handle("/api/disputes/", mw("operator", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/disputes/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || len(parts) != 2 || parts[1] != "resolve" {
			http.NotFound(w, r)
			return
		}
		c := claims(r)
		var in service.ResolveDisputeInput
		if !decode(w, r, &in) {
			return
		}
		if err := s.svc.ResolveDispute(r.Context(), c.Sub, id, in); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "resolved"})
	}))
	s.mux.Handle("/api/complaints", mw("resident", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		var in struct {
			AppointmentID *int64 `json:"appointment_id"`
			Category      string `json:"category"`
			Content       string `json:"content"`
		}
		if !decode(w, r, &in) {
			return
		}
		id, err := s.svc.CreateComplaint(r.Context(), c.Sub, in.AppointmentID, in.Category, in.Content)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]int64{"complaint_id": id})
	}))
	s.mux.Handle("/api/complaints/", mw("operator", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/complaints/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || len(parts) != 2 || parts[1] != "handle" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Response string `json:"response"`
		}
		if !decode(w, r, &in) {
			return
		}
		if err := s.svc.HandleComplaint(r.Context(), claims(r).Sub, id, in.Response); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "closed"})
	}))

	s.mux.Handle("/api/notifications", mw("resident", func(w http.ResponseWriter, r *http.Request) {
		c := claims(r)
		list, err := s.svc.Notifications(r.Context(), c.Sub, r.URL.Query().Get("unread") == "true")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, list)
	}))
	s.mux.Handle("/api/notifications/", mw("resident", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/notifications/")
		parts := strings.SplitN(rest, "/", 2)
		id, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || len(parts) != 2 || parts[1] != "read" {
			http.NotFound(w, r)
			return
		}
		if err := s.svc.MarkNotificationRead(r.Context(), claims(r).Sub, id); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "read"})
	}))

	s.mux.Handle("/api/street/dashboard", mw("street", func(w http.ResponseWriter, r *http.Request) {
		res, err := s.svc.StreetDashboard(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, res)
	}))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Phone    string `json:"phone"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	a, err := s.svc.Login(r.Context(), in.Phone, in.Password)
	if err != nil {
		writeErr(w, err)
		return
	}
	token := auth.Issue(s.secret, auth.Claims{Sub: a.ID, Role: a.Role, Name: a.Name})
	writeJSON(w, 200, map[string]any{
		"token": token, "account_id": a.ID, "role": a.Role, "name": a.Name,
	})
}

func requireRole(c auth.Claims, roles ...string) bool {
	for _, r := range roles {
		if c.Role == r {
			return true
		}
	}
	return false
}

// appointmentResource 处理 /api/appointments/{id}/{action}
func (s *Server) appointmentResource(w http.ResponseWriter, r *http.Request) {
	c := claims(r)
	id, action, err := apptIDFromPath("/api/appointments/", r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()

	if action == "" {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		detail, err := s.svc.AppointmentDetail(ctx, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		// 居民只能查看自己的预约；回收员/站点/街道/运营可查任意单
		if c.Role == "resident" {
			if a, ok := detail["appointment"].(*models.Appointment); ok && a.ResidentID != c.Sub {
				writeJSON(w, 403, map[string]string{"error": "无权查看他人预约"})
				return
			}
		}
		writeJSON(w, 200, detail)
		return
	}

	type actFn func(context.Context) (any, int, error)
	asRole := func(role string, fn actFn) {
		if c.Role != role {
			writeJSON(w, 403, map[string]string{"error": "需要 " + role + " 角色"})
			return
		}
		v, status, err := fn(ctx)
		if err != nil {
			writeErr(w, err)
			return
		}
		if status == 0 {
			status = 200
		}
		if v == nil {
			v = map[string]string{"status": "ok"}
		}
		writeJSON(w, status, v)
	}

	switch action {
	case "cancel":
		asRole("resident", func(ctx context.Context) (any, int, error) {
			return nil, 0, s.svc.CancelAppointment(ctx, c.Sub, id)
		})
	case "items":
		asRole("resident", func(ctx context.Context) (any, int, error) {
			var in service.ItemInput
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			v, err := s.svc.AddItem(ctx, c.Sub, id, in)
			return v, 201, err
		})
	case "qr":
		if !requireRole(c, "resident") {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return
		}
		tok, err := s.svc.QRCode(ctx, c.Sub, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"qr_token": tok})
	case "confirm-weigh":
		asRole("resident", func(ctx context.Context) (any, int, error) {
			var in struct {
				Agree  bool   `json:"agree"`
				Method string `json:"method"`
				Note   string `json:"note"`
			}
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			if err := s.svc.ConfirmWeigh(ctx, c.Sub, id, in.Agree, in.Method, in.Note); err != nil {
				return nil, 0, err
			}
			ok := "confirmed"
			if !in.Agree {
				ok = "disputed"
			}
			return map[string]string{"status": ok}, 200, nil
		})
	case "settle":
		asRole("resident", func(ctx context.Context) (any, int, error) {
			var in service.SettleInput
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			v, err := s.svc.Settle(ctx, c.Sub, id, in)
			return v, 200, err
		})
	case "settlements":
		if !requireRole(c, "resident", "operator", "street") {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return
		}
		list, err := s.svc.ListSettlements(ctx, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 200, list)

	case "accept":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			return nil, 0, s.svc.AcceptDispatch(ctx, c.Sub, id)
		})
	case "decline":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			var in struct {
				Reason string `json:"reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			return nil, 0, s.svc.DeclineDispatch(ctx, c.Sub, id, in.Reason)
		})
	case "enroute":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			return nil, 0, s.svc.Enroute(ctx, c.Sub, id)
		})
	case "arrive":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			v, err := s.svc.Arrival(ctx, c.Sub, id)
			return v, 200, err
		})
	case "verify":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			var in service.VerifyInput
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			return nil, 0, s.svc.VerifyResident(ctx, c.Sub, id, in)
		})
	case "weigh":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			var in service.WeighInput
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			v, err := s.svc.OnsiteWeigh(ctx, c.Sub, id, in)
			return v, 201, err
		})
	case "assist-confirm":
		asRole("recycler", func(ctx context.Context) (any, int, error) {
			var in struct {
				Note string `json:"note"`
			}
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			return nil, 0, s.svc.AssistConfirmWeigh(ctx, c.Sub, id, in.Note)
		})

	case "inbound":
		asRole("station", func(ctx context.Context) (any, int, error) {
			var in service.WeighInput
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			v, err := s.svc.InboundWeigh(ctx, c.Sub, id, in)
			return v, 201, err
		})
	case "certificate":
		if !requireRole(c, "station", "operator") {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return
		}
		cert, err := s.svc.IssueCertificate(ctx, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, cert)

	case "reoffer":
		asRole("operator", func(ctx context.Context) (any, int, error) {
			return nil, 0, s.svc.AutoOffer(ctx, id)
		})
	case "assign":
		asRole("operator", func(ctx context.Context) (any, int, error) {
			var in struct {
				RecyclerID int64 `json:"recycler_id"`
			}
			if !decode(w, r, &in) {
				return nil, 0, errDecode
			}
			return nil, 0, s.svc.ManualAssign(ctx, c.Sub, id, in.RecyclerID)
		})

	case "disputes":
		if !requireRole(c, "resident", "station", "recycler", "operator") {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return
		}
		var in struct {
			Type        string `json:"type"`
			Title       string `json:"title"`
			Description string `json:"description"`
		}
		if !decode(w, r, &in) {
			return
		}
		did, err := s.svc.RaiseDispute(ctx, id, c.Sub, in.Type, in.Title, in.Description)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, 201, map[string]int64{"dispute_id": did})

	default:
		http.NotFound(w, r)
	}
}

var errDecode = &service.BizError{Code: "BAD_JSON", Msg: "请求体 JSON 解析失败"}
