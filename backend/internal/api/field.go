package api

import (
	"context"
	"net/http"

	"github.com/agriproof/backend/internal/core"
)

// Field app endpoints. Every call is tied to the signed-in phone: staff act only inside their own
// cooperative and role, farmers see only their own records.

func (s *Server) fieldRoutes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		me, err := s.svc.WhoAmI(r.Context(), phoneFrom(r))
		s.respond(w, http.StatusOK, me, err)
	})
	m.HandleFunc("POST /me/logout", func(w http.ResponseWriter, r *http.Request) {
		token, viaCookie := memberToken(r)
		err := s.svc.EndPhoneSession(r.Context(), token)
		if viaCookie {
			http.SetCookie(w, &http.Cookie{Name: memberCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		}
		s.respond(w, http.StatusOK, map[string]string{"status": "signed out"}, err)
	})
	m.HandleFunc("GET /me/crops", func(w http.ResponseWriter, r *http.Request) {
		c, err := s.svc.Crops(r.Context())
		s.respond(w, http.StatusOK, c, err)
	})

	// ----- farmers -----
	m.HandleFunc("GET /me/farmer/{coop}", s.asFarmer(func(w http.ResponseWriter, r *http.Request, f core.Farmer) {
		ctx := r.Context()
		dl, err := s.svc.Deliveries(ctx, f.CoopID, "", f.ID, 100)
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		pay, err := s.svc.ListPayouts(ctx, f.ID, 50)
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		inputs, err := s.svc.Issues(ctx, f.CoopID, f.ID, 50)
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		ver, _ := s.svc.VerifyLedger(ctx, f.CoopID)
		for i := range pay {
			pay[i].Phone = "" // their own number; no need to echo it
		}
		writeJSON(w, http.StatusOK, map[string]any{"farmer": f, "deliveries": dl, "payouts": pay, "inputs": inputs, "verify": ver})
	}))
	m.HandleFunc("POST /me/check", s.limit(s.checkLimit, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code string `json:"code"`
		}
		if !decode(w, r, &in) {
			return
		}
		res, err := s.svc.CheckCode(r.Context(), in.Code, phoneFrom(r), "app", s.clientIP(r))
		s.respond(w, http.StatusOK, res, err)
	}))
	m.HandleFunc("POST /me/complaint", s.limit(s.checkLimit, func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code    string `json:"code"`
			BatchID string `json:"batch_id"`
			Problem string `json:"problem"`
		}
		if !decode(w, r, &in) {
			return
		}
		err := s.svc.Complain(r.Context(), phoneFrom(r), in.Code, in.BatchID, in.Problem)
		s.respond(w, http.StatusCreated, map[string]string{"status": "reported"}, err)
	}))

	// ----- cooperative staff -----
	m.HandleFunc("GET /me/coop/{coop}", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		ctx := r.Context()
		coop, err := s.svc.GetCoop(ctx, st.CoopID)
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		prices, _ := s.svc.Prices(ctx, st.CoopID)
		crops, _ := s.svc.Crops(ctx)
		funds, _ := s.svc.CoopFunds(ctx, st.CoopID)
		held, _ := s.svc.Deliveries(ctx, st.CoopID, "held", "", 100)
		heldFarmers, _ := s.svc.FindFarmers(ctx, st.CoopID, "", "held", 100)
		writeJSON(w, http.StatusOK, map[string]any{"me": st, "coop": coop, "prices": prices, "crops": crops, "funds": funds,
			"held_deliveries": held, "held_farmers": heldFarmers, "payments_mode": s.svc.PaymentsMode()})
	}))
	m.HandleFunc("GET /me/coop/{coop}/farmers", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		list, err := s.svc.FindFarmers(r.Context(), st.CoopID, r.URL.Query().Get("q"), r.URL.Query().Get("status"), qInt(r, "limit", 30))
		s.respond(w, http.StatusOK, list, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/farmers", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in core.FarmerInput
		if !decode(w, r, &in) {
			return
		}
		f, verdict, err := s.svc.RegisterFarmer(r.Context(), st, in)
		s.respond(w, http.StatusCreated, map[string]any{"farmer": f, "check": verdict}, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/farmers/{id}/approve", s.asStaff("manager", s.decideFarmer(true)))
	m.HandleFunc("POST /me/coop/{coop}/farmers/{id}/reject", s.asStaff("manager", s.decideFarmer(false)))
	m.HandleFunc("GET /me/coop/{coop}/farmers/{id}", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		f, err := s.svc.GetFarmer(r.Context(), r.PathValue("id"))
		if err == nil && f.CoopID != st.CoopID {
			err = core.ErrNotFound
		}
		s.respond(w, http.StatusOK, f, err)
	}))

	m.HandleFunc("POST /me/coop/{coop}/deliveries", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in core.DeliveryInput
		if !decode(w, r, &in) {
			return
		}
		d, err := s.svc.RecordDelivery(r.Context(), st, in)
		s.respond(w, http.StatusCreated, d, err)
	}))
	m.HandleFunc("GET /me/coop/{coop}/deliveries", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		list, err := s.svc.Deliveries(r.Context(), st.CoopID, r.URL.Query().Get("status"), r.URL.Query().Get("farmer"), qInt(r, "limit", 50))
		s.respond(w, http.StatusOK, list, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/deliveries/{id}/clear", s.asStaff("manager", s.reviewDelivery(true)))
	m.HandleFunc("POST /me/coop/{coop}/deliveries/{id}/reject", s.asStaff("manager", s.reviewDelivery(false)))
	m.HandleFunc("POST /me/coop/{coop}/deliveries/{id}/void", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		d, err := s.svc.VoidDelivery(r.Context(), st.CoopID, r.PathValue("id"), in.Reason, "manager:"+st.Name)
		s.respond(w, http.StatusOK, d, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/prices", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in struct {
			Crop       string  `json:"crop"`
			Grade      string  `json:"grade"`
			PricePerKg float64 `json:"price_per_kg"`
		}
		if !decode(w, r, &in) {
			return
		}
		err := s.svc.SetPrice(r.Context(), st.CoopID, in.Crop, in.Grade, in.PricePerKg, "manager:"+st.Name)
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		p, err := s.svc.Prices(r.Context(), st.CoopID)
		s.respond(w, http.StatusOK, p, err)
	}))

	// inputs
	m.HandleFunc("POST /me/coop/{coop}/inputs", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in struct {
			FarmerID string  `json:"farmer_id"`
			Code     string  `json:"code"`
			BatchID  string  `json:"batch_id"`
			Bags     float64 `json:"bags"`
		}
		if !decode(w, r, &in) {
			return
		}
		x, err := s.svc.IssueInput(r.Context(), st, in.FarmerID, in.Code, in.BatchID, in.Bags)
		s.respond(w, http.StatusCreated, x, err)
	}))
	m.HandleFunc("GET /me/coop/{coop}/inputs", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		list, err := s.svc.Issues(r.Context(), st.CoopID, r.URL.Query().Get("farmer"), qInt(r, "limit", 50))
		s.respond(w, http.StatusOK, list, err)
	}))
	m.HandleFunc("GET /me/coop/{coop}/batches", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		list, err := s.svc.CoopBatches(r.Context(), st.CoopID)
		s.respond(w, http.StatusOK, list, err)
	}))

	// money
	m.HandleFunc("GET /me/coop/{coop}/payruns", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		list, err := s.svc.Payruns(r.Context(), st.CoopID, 20)
		s.respond(w, http.StatusOK, list, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/payruns", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		p, err := s.svc.CreatePayrun(r.Context(), st)
		s.respond(w, http.StatusCreated, p, err)
	}))
	m.HandleFunc("GET /me/coop/{coop}/payruns/{id}", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		p, err := s.payrunIn(r.Context(), st.CoopID, r.PathValue("id"))
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		lines, err := s.svc.PayrunLines(r.Context(), p.ID)
		s.respond(w, http.StatusOK, map[string]any{"payrun": p, "lines": lines}, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/payruns/{id}/approve", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		if _, err := s.payrunIn(r.Context(), st.CoopID, r.PathValue("id")); err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		p, err := s.svc.ApprovePayrun(r.Context(), r.PathValue("id"), st.ID, "manager:"+st.Name)
		s.respond(w, http.StatusOK, p, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/payruns/{id}/cancel", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		p, err := s.svc.CancelPayrun(r.Context(), st.CoopID, r.PathValue("id"), "manager:"+st.Name)
		s.respond(w, http.StatusOK, p, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/fund", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in struct {
			Amount int64 `json:"amount"`
		}
		if !decode(w, r, &in) {
			return
		}
		// The prompt goes to the manager's own phone.
		f, err := s.svc.StartFunding(r.Context(), st.CoopID, st.Phone, in.Amount, "manager:"+st.Name)
		s.respond(w, http.StatusAccepted, f, err)
	}))

	// lots
	m.HandleFunc("GET /me/coop/{coop}/lots", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		list, err := s.svc.Lots(r.Context(), st.CoopID)
		s.respond(w, http.StatusOK, list, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/lots", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in struct {
			Crop  string `json:"crop"`
			Buyer string `json:"buyer"`
		}
		if !decode(w, r, &in) {
			return
		}
		l, err := s.svc.CreateLot(r.Context(), st.CoopID, in.Crop, in.Buyer, "manager:"+st.Name)
		s.respond(w, http.StatusCreated, l, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/lots/{id}/add", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		var in struct {
			DeliveryIDs []string `json:"delivery_ids"`
		}
		if !decode(w, r, &in) {
			return
		}
		l, err := s.svc.AddToLot(r.Context(), st.CoopID, r.PathValue("id"), in.DeliveryIDs)
		s.respond(w, http.StatusOK, l, err)
	}))
	m.HandleFunc("POST /me/coop/{coop}/lots/{id}/close", s.asStaff("manager", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		l, err := s.svc.CloseLot(r.Context(), st.CoopID, r.PathValue("id"))
		s.respond(w, http.StatusOK, l, err)
	}))
	m.HandleFunc("GET /me/coop/{coop}/verify", s.asStaff("", func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		rep, err := s.svc.VerifyLedger(r.Context(), st.CoopID)
		s.respond(w, http.StatusOK, rep, err)
	}))
	return m
}

func (s *Server) asStaff(role string, h func(http.ResponseWriter, *http.Request, core.Staff)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := s.svc.StaffIn(r.Context(), phoneFrom(r), r.PathValue("coop"))
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		if role != "" && st.Role != role {
			s.respond(w, 0, nil, wrap(core.ErrForbidden, "only a manager can do this"))
			return
		}
		h(w, r, st)
	}
}

func (s *Server) asFarmer(h func(http.ResponseWriter, *http.Request, core.Farmer)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := s.svc.FarmerFor(r.Context(), phoneFrom(r), r.PathValue("coop"))
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		h(w, r, f)
	}
}

func (s *Server) decideFarmer(approve bool) func(http.ResponseWriter, *http.Request, core.Staff) {
	return func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		f, err := s.svc.GetFarmer(r.Context(), r.PathValue("id"))
		if err == nil && f.CoopID != st.CoopID {
			err = core.ErrNotFound
		}
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		f, err = s.svc.DecideFarmer(r.Context(), f.ID, approve, "manager:"+st.Name, st.ID)
		s.respond(w, http.StatusOK, f, err)
	}
}

func (s *Server) reviewDelivery(approve bool) func(http.ResponseWriter, *http.Request, core.Staff) {
	return func(w http.ResponseWriter, r *http.Request, st core.Staff) {
		d, err := s.svc.ReviewDelivery(r.Context(), st.CoopID, r.PathValue("id"), approve, "manager:"+st.Name)
		s.respond(w, http.StatusOK, d, err)
	}
}

func (s *Server) payrunIn(ctx context.Context, coopID, id string) (core.Payrun, error) {
	p, err := s.svc.GetPayrun(ctx, id)
	if err == nil && p.CoopID != coopID {
		return p, core.ErrNotFound
	}
	return p, err
}
