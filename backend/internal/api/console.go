package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"

	"github.com/agriproof/backend/internal/core"
)

// Console endpoints for operators (signed-in admin, or the service key).

func (s *Server) consoleRoutes() http.Handler {
	m := http.NewServeMux()
	get := func(path string, f func(r *http.Request) (any, error)) {
		m.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			v, err := f(r)
			s.respond(w, http.StatusOK, v, err)
		})
	}
	by := func(r *http.Request) string { a := actorFrom(r); return a.Type + ":" + a.Name }

	m.HandleFunc("GET /admin/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, actorFrom(r)) })
	get("/audit", func(r *http.Request) (any, error) { return s.svc.AuditLog(r.Context(), qInt(r, "limit", 100)) })
	get("/system", func(r *http.Request) (any, error) { return s.svc.SystemStatus(r.Context()), nil })
	get("/stats", func(r *http.Request) (any, error) { return s.svc.Stats(r.Context()) })
	get("/crops", func(r *http.Request) (any, error) { return s.svc.Crops(r.Context()) })

	// cooperatives
	get("/coops", func(r *http.Request) (any, error) { return s.svc.ListCoops(r.Context()) })
	m.HandleFunc("POST /coops", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, County string }
		if !decode(w, r, &in) {
			return
		}
		c, err := s.svc.CreateCoop(r.Context(), in.Name, in.County)
		s.respond(w, http.StatusCreated, c, err)
	})
	get("/coops/{id}", func(r *http.Request) (any, error) {
		ctx, id := r.Context(), r.PathValue("id")
		c, err := s.svc.GetCoop(ctx, id)
		if err != nil {
			return nil, err
		}
		centres, _ := s.svc.Centres(ctx, id)
		staff, _ := s.svc.StaffList(ctx, id)
		prices, _ := s.svc.Prices(ctx, id)
		funds, _ := s.svc.CoopFunds(ctx, id)
		return map[string]any{"coop": c, "centres": centres, "staff": staff, "prices": prices, "funds": funds}, nil
	})
	m.HandleFunc("POST /coops/{id}/centres", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Location string }
		if !decode(w, r, &in) {
			return
		}
		c, err := s.svc.AddCentre(r.Context(), r.PathValue("id"), in.Name, in.Location)
		s.respond(w, http.StatusCreated, c, err)
	})
	m.HandleFunc("POST /coops/{id}/staff", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CentreID string `json:"centre_id"`
			Name     string `json:"name"`
			Phone    string `json:"phone"`
			Role     string `json:"role"`
		}
		if !decode(w, r, &in) {
			return
		}
		st, err := s.svc.AddStaff(r.Context(), r.PathValue("id"), in.CentreID, in.Name, in.Phone, in.Role)
		s.respond(w, http.StatusCreated, st, err)
	})
	m.HandleFunc("POST /staff/{id}/deactivate", func(w http.ResponseWriter, r *http.Request) {
		err := s.svc.SetStaffActive(r.Context(), r.PathValue("id"), false)
		s.respond(w, http.StatusOK, map[string]string{"status": "removed"}, err)
	})
	m.HandleFunc("POST /coops/{id}/prices", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Crop       string  `json:"crop"`
			Grade      string  `json:"grade"`
			PricePerKg float64 `json:"price_per_kg"`
		}
		if !decode(w, r, &in) {
			return
		}
		err := s.svc.SetPrice(r.Context(), r.PathValue("id"), in.Crop, in.Grade, in.PricePerKg, by(r))
		if err != nil {
			s.respond(w, 0, nil, err)
			return
		}
		p, err := s.svc.Prices(r.Context(), r.PathValue("id"))
		s.respond(w, http.StatusOK, p, err)
	})
	get("/coops/{id}/farmers", func(r *http.Request) (any, error) {
		return s.svc.FindFarmers(r.Context(), r.PathValue("id"), r.URL.Query().Get("q"), r.URL.Query().Get("status"), qInt(r, "limit", 100))
	})
	get("/coops/{id}/deliveries", func(r *http.Request) (any, error) {
		return s.svc.Deliveries(r.Context(), r.PathValue("id"), r.URL.Query().Get("status"), r.URL.Query().Get("farmer"), qInt(r, "limit", 100))
	})
	get("/coops/{id}/ledger", func(r *http.Request) (any, error) {
		return s.svc.Ledger(r.Context(), r.PathValue("id"), qInt(r, "limit", 60))
	})
	get("/coops/{id}/verify", func(r *http.Request) (any, error) { return s.svc.VerifyLedger(r.Context(), r.PathValue("id")) })
	get("/coops/{id}/lots", func(r *http.Request) (any, error) { return s.svc.Lots(r.Context(), r.PathValue("id")) })
	get("/coops/{id}/inputs", func(r *http.Request) (any, error) {
		return s.svc.Issues(r.Context(), r.PathValue("id"), "", qInt(r, "limit", 100))
	})

	// AI holds
	get("/farmers/held", func(r *http.Request) (any, error) { return s.svc.HeldFarmers(r.Context()) })
	m.HandleFunc("POST /farmers/{id}/approve", s.adminDecideFarmer(true))
	m.HandleFunc("POST /farmers/{id}/reject", s.adminDecideFarmer(false))
	get("/deliveries/held", func(r *http.Request) (any, error) { return s.svc.HeldDeliveries(r.Context()) })
	m.HandleFunc("POST /deliveries/{id}/clear", s.adminReview(true))
	m.HandleFunc("POST /deliveries/{id}/reject", s.adminReview(false))

	// inputs
	get("/suppliers", func(r *http.Request) (any, error) { return s.svc.Suppliers(r.Context()) })
	m.HandleFunc("POST /suppliers", func(w http.ResponseWriter, r *http.Request) {
		var in core.SupplierInput
		if !decode(w, r, &in) {
			return
		}
		x, err := s.svc.AddSupplier(r.Context(), in)
		s.respond(w, http.StatusCreated, x, err)
	})
	m.HandleFunc("POST /suppliers/{id}/verify", s.supplierStatus("verified"))
	m.HandleFunc("POST /suppliers/{id}/suspend", s.supplierStatus("suspended"))
	get("/batches", func(r *http.Request) (any, error) { return s.svc.Batches(r.Context()) })
	m.HandleFunc("POST /batches", func(w http.ResponseWriter, r *http.Request) {
		var in core.BatchInput
		if !decode(w, r, &in) {
			return
		}
		b, err := s.svc.AddBatch(r.Context(), in, by(r))
		s.respond(w, http.StatusCreated, b, err)
	})
	m.HandleFunc("POST /batches/{id}/codes", s.issueCodes)
	m.HandleFunc("POST /batches/{id}/lab", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Result  string `json:"result"`
			Content string `json:"content"`
			Ref     string `json:"ref"`
		}
		if !decode(w, r, &in) {
			return
		}
		b, err := s.svc.RecordLab(r.Context(), r.PathValue("id"), in.Result, in.Content, in.Ref)
		s.respond(w, http.StatusOK, b, err)
	})
	m.HandleFunc("POST /batches/{id}/ship", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CoopID string `json:"coop_id"`
			ToName string `json:"to_name"`
			Bags   int    `json:"bags"`
		}
		if !decode(w, r, &in) {
			return
		}
		b, err := s.svc.AddShipment(r.Context(), r.PathValue("id"), in.CoopID, in.ToName, in.Bags, by(r))
		s.respond(w, http.StatusOK, b, err)
	})
	m.HandleFunc("POST /batches/{id}/recall", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		b, told, err := s.svc.RecallBatch(r.Context(), r.PathValue("id"), in.Reason, by(r))
		s.respond(w, http.StatusOK, map[string]any{"batch": b, "farmers_told": told}, err)
	})
	get("/batches/{id}/harvest", func(r *http.Request) (any, error) { return s.svc.BatchHarvest(r.Context(), r.PathValue("id")) })
	get("/checks/suspicious", func(r *http.Request) (any, error) { return s.svc.SuspiciousChecks(r.Context(), qInt(r, "limit", 50)) })
	get("/complaints", func(r *http.Request) (any, error) { return s.svc.Complaints(r.Context(), qInt(r, "limit", 50)) })

	// money
	get("/fundings", func(r *http.Request) (any, error) { return s.svc.Fundings(r.Context(), qInt(r, "limit", 50)) })
	m.HandleFunc("POST /fundings/{id}/confirm", s.decideFunding(true))
	m.HandleFunc("POST /fundings/{id}/reject", s.decideFunding(false))
	m.HandleFunc("POST /coops/{id}/fund", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Phone  string `json:"phone"`
			Amount int64  `json:"amount"`
		}
		if !decode(w, r, &in) {
			return
		}
		f, err := s.svc.StartFunding(r.Context(), r.PathValue("id"), in.Phone, in.Amount, by(r))
		s.respond(w, http.StatusAccepted, f, err)
	})
	get("/payruns", func(r *http.Request) (any, error) {
		return s.svc.Payruns(r.Context(), r.URL.Query().Get("coop"), qInt(r, "limit", 50))
	})
	get("/payruns/{id}", func(r *http.Request) (any, error) {
		p, err := s.svc.GetPayrun(r.Context(), r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		lines, err := s.svc.PayrunLines(r.Context(), p.ID)
		return map[string]any{"payrun": p, "lines": lines}, err
	})
	m.HandleFunc("POST /payruns/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.svc.ApprovePayrun(r.Context(), r.PathValue("id"), "", by(r))
		s.respond(w, http.StatusOK, p, err)
	})
	m.HandleFunc("POST /payruns/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		p, err := s.svc.GetPayrun(r.Context(), r.PathValue("id"))
		if err == nil {
			p, err = s.svc.CancelPayrun(r.Context(), p.CoopID, p.ID, by(r))
		}
		s.respond(w, http.StatusOK, p, err)
	})
	get("/payouts", func(r *http.Request) (any, error) { return s.svc.ListPayouts(r.Context(), "", qInt(r, "limit", 100)) })
	m.HandleFunc("POST /payouts/{id}/mark-paid", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Receipt string `json:"receipt"`
		}
		if !decode(w, r, &in) {
			return
		}
		err := s.svc.MarkPayoutPaid(r.Context(), r.PathValue("id"), in.Receipt, by(r))
		s.respond(w, http.StatusOK, map[string]string{"status": "paid"}, err)
	})
	m.HandleFunc("POST /payouts/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		err := s.svc.RetryPayout(r.Context(), r.PathValue("id"), in.Reason, by(r))
		s.respond(w, http.StatusOK, map[string]string{"status": "queued"}, err)
	})
	m.HandleFunc("POST /payouts/{id}/return", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		err := s.svc.ReturnPayout(r.Context(), r.PathValue("id"), in.Reason, by(r))
		s.respond(w, http.StatusOK, map[string]string{"status": "returned"}, err)
	})
	get("/reconciliation", func(r *http.Request) (any, error) {
		rec, err := s.svc.LatestReconciliation(r.Context())
		return map[string]any{"latest": rec, "provider": s.svc.PaymentProvider(), "mode": s.svc.PaymentsMode()}, err
	})
	m.HandleFunc("POST /reconciliation/run", func(w http.ResponseWriter, r *http.Request) {
		rec, err := s.svc.Reconcile(r.Context())
		s.respond(w, http.StatusOK, rec, err)
	})
	return m
}

func (s *Server) adminDecideFarmer(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := actorFrom(r)
		f, err := s.svc.DecideFarmer(r.Context(), r.PathValue("id"), approve, a.Type+":"+a.Name, "")
		s.respond(w, http.StatusOK, f, err)
	}
}

func (s *Server) adminReview(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := actorFrom(r)
		d, err := s.svc.GetDelivery(r.Context(), r.PathValue("id"))
		if err == nil {
			d, err = s.svc.ReviewDelivery(r.Context(), d.CoopID, d.ID, approve, a.Type+":"+a.Name)
		}
		s.respond(w, http.StatusOK, d, err)
	}
}

func (s *Server) supplierStatus(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := actorFrom(r)
		x, err := s.svc.SetSupplierStatus(r.Context(), r.PathValue("id"), status, a.Type+":"+a.Name)
		s.respond(w, http.StatusOK, x, err)
	}
}

func (s *Server) decideFunding(confirm bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := actorFrom(r)
		err := s.svc.DecideTestFunding(r.Context(), r.PathValue("id"), confirm, a.Type+":"+a.Name)
		s.respond(w, http.StatusOK, map[string]bool{"confirmed": confirm}, err)
	}
}

// issueCodes returns the bag codes once, as a CSV file for the label printer.
func (s *Server) issueCodes(w http.ResponseWriter, r *http.Request) {
	codes, b, err := s.svc.IssueCodes(r.Context(), r.PathValue("id"))
	if err != nil {
		s.respond(w, 0, nil, err)
		return
	}
	base := s.publicURL
	if base == "" {
		scheme := "http"
		if s.isHTTPS(r) {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	name := strings.Map(func(c rune) rune {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' {
			return c
		}
		return '-'
	}, b.Product+"-"+b.BatchNo)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="bag-codes-%s.csv"`, name))
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"serial", "code", "check_link", "product", "batch", "supplier"})
	for _, c := range codes {
		_ = cw.Write([]string{fmt.Sprint(c.Serial), c.Code, base + "/check/" + strings.ReplaceAll(c.Code, "-", ""),
			safeCell(b.Product), safeCell(b.BatchNo), safeCell(b.SupplierName)})
	}
	cw.Flush()
}

// safeCell stops spreadsheet programs from running a cell as a formula (for example a product
// name typed as =HYPERLINK(...)) when the print shop opens the codes file.
func safeCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
