package kora

import (
	"net/url"
	"strings"
)

// Kora API paths, relative to the base URL (https://api.korapay.com/merchant).
// Test and live are told apart by the secret key, not by the host.
//
// pathBankTransfer is the one path not printed in Kora's own text guide (it
// links to their Postman reference), so it was taken from convention. The first
// sandbox call settles it: POST /kora/charge.
const (
	pathBankTransfer = "/api/v1/charges/bank-transfer"
	pathChargePrefix = "/api/v1/charges/"
)

// pathCharge is the Transaction Query endpoint: look a charge up by the
// reference we gave it.
func pathCharge(reference string) string {
	return pathChargePrefix + url.PathEscape(reference)
}

// operationName is the label used in log lines.
func operationName(method, path string) string {
	// The base URL may carry a prefix (/merchant), so match the end of the path.
	switch {
	case strings.HasSuffix(path, pathBankTransfer):
		return "CreateCharge"
	case method == "GET" && strings.Contains(path, pathChargePrefix):
		return "GetCharge"
	}
	return path
}
