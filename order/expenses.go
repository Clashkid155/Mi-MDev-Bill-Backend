package order

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"Bills-Backend/reply"
)

// Expense is money the business spent that no order records.
type Expense struct {
	ID         int64     `json:"id"`
	Day        string    `json:"day"` // the Nigerian day it belongs to, YYYY-MM-DD
	Category   string    `json:"category"`
	Label      string    `json:"label"`
	AmountKobo int64     `json:"amount_kobo"`
	Note       string    `json:"note,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// CategoryTotal is what was spent in one category.
type CategoryTotal struct {
	Category  string `json:"category"`
	TotalKobo int64  `json:"total_kobo"`
	Entries   int    `json:"entries"`
}

func (s *Store) AddExpense(ctx context.Context, e Expense) (Expense, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO expenses (day, category, label, amount_kobo, note, created_at) VALUES (?,?,?,?,?,?)`,
		e.Day, e.Category, e.Label, e.AmountKobo, e.Note, now.Unix())
	if err != nil {
		return Expense{}, err
	}
	e.ID, _ = res.LastInsertId()
	e.CreatedAt = time.Unix(now.Unix(), 0)
	return e, nil
}

// ListExpenses returns the expenses whose day falls between from and to (both
// YYYY-MM-DD, inclusive), newest day first.
func (s *Store) ListExpenses(ctx context.Context, from, to string) ([]Expense, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, day, category, label, amount_kobo, note, created_at FROM expenses
		WHERE day >= ? AND day <= ? ORDER BY day DESC, id DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Expense{}
	for rows.Next() {
		var e Expense
		var created int64
		if err := rows.Scan(&e.ID, &e.Day, &e.Category, &e.Label, &e.AmountKobo, &e.Note, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) DeleteExpense(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM expenses WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// expensesBetween adds up the expenses in a window, in total and by category.
func (s *Store) expensesBetween(ctx context.Context, from, to string) (total int64, byCategory []CategoryTotal, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT category, COALESCE(SUM(amount_kobo), 0), COUNT(*) FROM expenses
		WHERE day >= ? AND day <= ? GROUP BY category ORDER BY 2 DESC, 1`, from, to)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	byCategory = []CategoryTotal{}
	for rows.Next() {
		var c CategoryTotal
		if err := rows.Scan(&c.Category, &c.TotalKobo, &c.Entries); err != nil {
			return 0, nil, err
		}
		total += c.TotalKobo
		byCategory = append(byCategory, c)
	}
	return total, byCategory, rows.Err()
}

// addExpense records money spent: POST /admin/expenses
//
//	{"day":"2026-10-20","category":"hosting","label":"Server for October","amount_kobo":1500000,"note":""}
//
// Only category, label and amount_kobo are required; day is today (Nigerian time)
// unless given. Categories are free text so new kinds need no change here;
// hosting, email, agent_fee, domain and transfer_duty are the ones in use.
func (a *API) addExpense(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Day        string `json:"day"`
		Category   string `json:"category"`
		Label      string `json:"label"`
		AmountKobo int64  `json:"amount_kobo"`
		Note       string `json:"note"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil || json.Unmarshal(body, &req) != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Category, req.Label, req.Note = strings.ToLower(strings.TrimSpace(req.Category)), strings.TrimSpace(req.Label), strings.TrimSpace(req.Note)
	switch {
	case req.Category == "" || len([]rune(req.Category)) > 40:
		reply.Fail(w, http.StatusBadRequest, "category is required (at most 40 characters), for example hosting")
		return
	case req.Label == "" || len([]rune(req.Label)) > 100:
		reply.Fail(w, http.StatusBadRequest, "label is required (at most 100 characters)")
		return
	case req.AmountKobo <= 0:
		reply.Fail(w, http.StatusBadRequest, "amount_kobo must be more than 0")
		return
	case len([]rune(req.Note)) > 500:
		reply.Fail(w, http.StatusBadRequest, "note must be at most 500 characters")
		return
	}
	if req.Day == "" {
		req.Day = time.Now().In(lagos).Format(dateLayout)
	} else if _, err := time.ParseInLocation(dateLayout, req.Day, lagos); err != nil {
		reply.Fail(w, http.StatusBadRequest, "day must be a day like 2026-10-20")
		return
	}
	e, err := a.Store.AddExpense(r.Context(), Expense{Day: req.Day, Category: req.Category, Label: req.Label, AmountKobo: req.AmountKobo, Note: req.Note})
	if err != nil {
		a.Log.Error("AddExpense - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, e)
}

// listExpenses shows the expenses of a window, with totals:
// GET /admin/expenses?from=2026-10-01&to=2026-10-31 (default the last 30 days).
func (a *API) listExpenses(w http.ResponseWriter, r *http.Request) {
	from, to, ok := windowOf(w, r)
	if !ok {
		return
	}
	fromDay, toDay := from.Format(dateLayout), to.Format(dateLayout)
	expenses, err := a.Store.ListExpenses(r.Context(), fromDay, toDay)
	if err != nil {
		a.Log.Error("ListExpenses - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	total, byCategory, err := a.Store.expensesBetween(r.Context(), fromDay, toDay)
	if err != nil {
		a.Log.Error("ListExpenses - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
		return
	}
	reply.OK(w, http.StatusOK, map[string]any{"from": fromDay, "to": toDay, "total_kobo": total, "by_category": byCategory, "expenses": expenses})
}

// deleteExpense removes a wrongly entered expense: DELETE /admin/expenses/{id}.
func (a *API) deleteExpense(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		reply.Fail(w, http.StatusBadRequest, "the expense id is a number")
		return
	}
	switch err := a.Store.DeleteExpense(r.Context(), id); {
	case errors.Is(err, ErrNotFound):
		reply.Fail(w, http.StatusNotFound, "expense not found")
	case err != nil:
		a.Log.Error("DeleteExpense - API", "detail", err.Error())
		reply.Fail(w, http.StatusInternalServerError, "internal error")
	default:
		reply.OKWithMessage(w, http.StatusOK, "expense deleted", nil)
	}
}
