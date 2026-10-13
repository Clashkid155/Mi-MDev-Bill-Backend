package email

import (
	"encoding/json"
	"net/http"

	"Bills-Backend/reply"
)

// RegisterDirectRoutes exposes Send as an HTTP endpoint so ZeptoMail can be used
// directly with curl or Postman. Whoever mounts this must protect it (see
// requireAdmin in main).
//
//	POST /email/send    {"to","to_name","subject","text","html"}
//
// to and subject are required; at least one of text or html is required.
func (c *Client) RegisterDirectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /email/send", c.handleSend)
}

func (c *Client) handleSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		To      string `json:"to"`
		ToName  string `json:"to_name"`
		Subject string `json:"subject"`
		Text    string `json:"text"`
		HTML    string `json:"html"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		reply.Fail(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.To == "" || in.Subject == "" {
		reply.Fail(w, http.StatusBadRequest, "to and subject are required")
		return
	}
	if in.Text == "" && in.HTML == "" {
		reply.Fail(w, http.StatusBadRequest, "text or html is required")
		return
	}
	err := c.Send(r.Context(), Message{To: in.To, ToName: in.ToName, Subject: in.Subject, Text: in.Text, HTML: in.HTML})
	if err != nil {
		reply.Fail(w, http.StatusBadGateway, err.Error())
		return
	}
	message := "sent"
	if c.sandbox {
		message = "sandbox: logged, not sent"
	}
	reply.OKWithMessage(w, http.StatusOK, message, map[string]bool{"sandbox": c.sandbox})
}
