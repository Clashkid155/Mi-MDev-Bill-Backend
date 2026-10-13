package abs

// ABS Vendor Direct API paths, relative to the base URL.
const (
	pathBuyAirtime    = "/api/user/transaction/buy_airtime.php"
	pathBuyData       = "/api/user/transaction/buy_data.php"
	pathBuyElectric   = "/api/user/transaction/buy_electricity.php"
	pathBuyVoucher    = "/api/user/transaction/buy_airtime_voucher.php"
	pathValidateMeter = "/api/user/transaction/validate_meter.php"
	pathSystemData    = "/api/user/system/get_system_data.php"

	// ABS's docs group this under the Partner API, not Direct, but it takes the
	// same Direct API key; the only way to look an order up afterwards.
	pathTransaction = "/api/partner/v1/transaction.php"
)

// operationNames are the labels used in log lines.
var operationNames = map[string]string{
	pathBuyAirtime:    "BuyAirtime",
	pathBuyData:       "BuyData",
	pathBuyElectric:   "BuyElectricity",
	pathBuyVoucher:    "BuyVoucher",
	pathValidateMeter: "ValidateMeter",
	pathSystemData:    "GetSystemData",
	pathTransaction:   "CheckTransaction",
}

func operationName(path string) string {
	if name, ok := operationNames[path]; ok {
		return name
	}
	return path
}
