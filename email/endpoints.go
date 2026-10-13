package email

// ZeptoMail API paths, relative to the base URL.
const pathSend = "/v1.1/email"

// operationName is the label used in log lines.
func operationName(path string) string {
	if path == pathSend {
		return "Send"
	}
	return path
}
