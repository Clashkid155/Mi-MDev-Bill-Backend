package abs

import (
	"encoding/json"
	"net/http"

	"Bills-Backend/reply"
)

// RegisterDirectRoutes exposes each ABS call as an HTTP endpoint so the ABS API
// can be used directly with curl or Postman, without going through Monnify. The
// ABS sandbox setting decides whether a purchase is only validated or really
// spent. Whoever mounts these must protect them (see requireAdmin in main),
// because they can spend the ABS wallet.
//
//	GET  /abs/catalog
//	POST /abs/airtime         {"network","phone","amount_naira"}
//	POST /abs/data            {"network","plan","phone"}
//	POST /abs/electricity     {"disco","meter","amount_naira"}
//	POST /abs/voucher         {"network","unit","quantity"}
//	POST /abs/validate-meter  {"disco","meter"}
//	GET  /abs/transaction/{ident}
func (c *Client) RegisterDirectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /abs/catalog", c.handleCatalog)
	mux.HandleFunc("POST /abs/airtime", c.handleAirtime)
	mux.HandleFunc("POST /abs/data", c.handleData)
	mux.HandleFunc("POST /abs/electricity", c.handleElectricity)
	mux.HandleFunc("POST /abs/voucher", c.handleVoucher)
	mux.HandleFunc("POST /abs/validate-meter", c.handleValidateMeter)
	mux.HandleFunc("GET /abs/transaction/{ident}", c.handleCheckTransaction)
}

// spendData is what every purchase/lookup endpoint puts in the envelope's
// "data" field. Raw is ABS's own reply, unchanged, so you see exactly what
// ABS said.
type spendData struct {
	Sandbox bool  `json:"sandbox"`
	State   State `json:"state"`
	Raw     any   `json:"raw"`
}

// reply writes result as a successful envelope: the HTTP call to ABS was
// completed, whatever ABS itself decided (State/Message describe that).
func (c *Client) reply(w http.ResponseWriter, result Result) {
	var raw any = string(result.Raw)
	if json.Valid(result.Raw) {
		raw = json.RawMessage(result.Raw)
	}
	reply.OKWithMessage(w, http.StatusOK, result.Message, spendData{Sandbox: c.sandbox, State: result.State, Raw: raw})
}

func (c *Client) handleCatalog(w http.ResponseWriter, r *http.Request) {
	data, err := c.SystemData(r.Context())
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OK(w, http.StatusOK, json.RawMessage(data))
}

func (c *Client) handleAirtime(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Network     string `json:"network"`
		Phone       string `json:"phone"`
		AmountNaira int64  `json:"amount_naira"`
	}
	if !decode(w, r, &in) {
		return
	}
	c.reply(w, c.Airtime(r.Context(), in.Network, in.Phone, in.AmountNaira*100))
}

func (c *Client) handleData(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Network string `json:"network"`
		Plan    string `json:"plan"`
		Phone   string `json:"phone"`
	}
	if !decode(w, r, &in) {
		return
	}
	c.reply(w, c.Data(r.Context(), in.Network, in.Plan, in.Phone))
}

func (c *Client) handleElectricity(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disco       string `json:"disco"`
		Meter       string `json:"meter"`
		AmountNaira int64  `json:"amount_naira"`
	}
	if !decode(w, r, &in) {
		return
	}
	c.reply(w, c.Electricity(r.Context(), in.Disco, in.Meter, in.AmountNaira*100))
}

func (c *Client) handleVoucher(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Network  string `json:"network"`
		Unit     string `json:"unit"`
		Quantity int    `json:"quantity"`
	}
	if !decode(w, r, &in) {
		return
	}
	c.reply(w, c.Voucher(r.Context(), in.Network, in.Unit, in.Quantity))
}

func (c *Client) handleCheckTransaction(w http.ResponseWriter, r *http.Request) {
	result, err := c.CheckTransaction(r.Context(), r.PathValue("ident"))
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	c.reply(w, result)
}

func (c *Client) handleValidateMeter(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disco string `json:"disco"`
		Meter string `json:"meter"`
	}
	if !decode(w, r, &in) {
		return
	}
	data, err := c.ValidateMeter(r.Context(), in.Disco, in.Meter)
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	reply.OK(w, http.StatusOK, json.RawMessage(data))
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(into); err != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}
