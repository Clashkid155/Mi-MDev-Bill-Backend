package monnify

import (
	"net/url"
	"strings"
)

// Monnify API paths, relative to the base URL.
const (
	pathLogin         = "/api/v1/auth/login"
	pathCreateInvoice = "/api/v1/invoice/create"
)

func pathInvoiceDetails(reference string) string {
	return "/api/v1/invoice/" + url.PathEscape(reference) + "/details"
}

func pathInvoiceCancel(reference string) string {
	return "/api/v1/invoice/" + url.PathEscape(reference) + "/cancel"
}

// operationName is the label used in log lines.
func operationName(path string) string {
	switch {
	case path == pathLogin:
		return "Login"
	case path == pathCreateInvoice:
		return "CreateInvoice"
	case strings.HasSuffix(path, "/details"):
		return "GetInvoice"
	case strings.HasSuffix(path, "/cancel"):
		return "CancelInvoice"
	}
	return path
}
