#!/usr/bin/env bash
# Tries what automated attackers try first against the live site and reports anything that
# answers differently from how it should: leaked files, debug pages, path tricks, injection
# strings, odd HTTP methods, oversized bodies. Read only; about 45 requests.
#
#   deploy/probe.sh https://api.dizztech.com
#
# Silent about what is fine; lists only the unexpected answers, then a count.
set -uo pipefail
H=${1:?usage: probe.sh https://your.host}
n=0
bad=0
body=$(mktemp)
big=$(mktemp)
trap 'rm -f "$body" "$big"' EXIT

check() { # label, expected status regex, curl arguments...
	local label=$1 want=$2 code leak st
	shift 2
	code=$(curl -s -o "$body" -m 20 -w '%{http_code}' "$@" 2>/dev/null)
	n=$((n + 1))
	leak=$(grep -ciE 'panic|goroutine|runtime error|sqlite|sql syntax|/Users/|/opt/bills|stack trace|<script>alert' "$body" 2>/dev/null)
	if echo "$code" | grep -qE "^($want)$" && [ "$leak" = 0 ]; then st=ok; else st=UNEXPECTED; bad=$((bad + 1)); fi
	[ $st = ok ] || printf '  %-10s %s  %s\n' "$st" "$code" "$label"
}

for p in /.env /.git/config /.git/HEAD /config.json /bills.db /bills.db-wal /backup.sql /server-status /phpinfo.php /wp-login.php /wp-admin/ /actuator/env /debug/pprof/ /debug/vars /metrics /swagger.json /openapi.json /api-docs /admin/ /console /.DS_Store /id_rsa /etc/passwd; do
	check "GET $p" "404|401|403" "$H$p"
done
# Wrong tokens count toward the admin lockout (5 in a row blocks the address for 15 minutes), so
# this script makes only three admin requests: /admin/, the SQL filter and one wrong Bearer.
check "GET /admin (redirects to /admin/, which needs the token)" "307|308|404|401" "$H/admin"
check "GET /../../etc/passwd" "404|400|401" --path-as-is "$H/../../etc/passwd"
check "GET /%2e%2e/%2e%2e/etc/passwd" "404|400|401" --path-as-is "$H/%2e%2e/%2e%2e/etc/passwd"
check "GET /v1/orders/..%2f..%2fadmin%2forders" "404|400|401" --path-as-is "$H/v1/orders/..%2f..%2fadmin%2forders"
check "GET /v1/orders/%00" "404|400" --path-as-is "$H/v1/orders/%00"
check "GET //admin//orders" "404|401|307|308" --path-as-is "$H//admin//orders"
check "GET /ADMIN/orders" "404|401" "$H/ADMIN/orders"
check "order id with a SQL quote" "404|400" "$H/v1/orders/1'%20OR%20'1'='1"
check "order id with UNION SELECT" "404|400" "$H/v1/orders/x%27%20UNION%20SELECT%20*%20FROM%20orders--"
check "order id with a script tag" "404|400" "$H/v1/orders/%3Cscript%3Ealert(1)%3C/script%3E"
check "admin filter with SQL" "401" "$H/admin/orders?status=%27%3B%20DROP%20TABLE%20orders%3B--"
check "order id 5 KB long" "404|400|414" "$H/v1/orders/$(head -c 5000 /dev/zero | tr '\0' 'A')"
check "TRACE /" "405|404|400|403" -X TRACE "$H/"
check "DELETE /healthz" "405|404" -X DELETE "$H/healthz"
check "PUT /v1/orders" "405|404" -X PUT "$H/v1/orders"
check "GET /webhooks/kora" "405|404" "$H/webhooks/kora"
head -c 3000000 /dev/zero | tr '\0' 'a' >"$big"
check "POST 3 MB body" "413|400" -X POST -H 'Content-Type: application/json' --data-binary @"$big" "$H/v1/orders"
check "POST invalid JSON" "400" -X POST -H 'Content-Type: application/json' -d '{"idempotency_key":' "$H/v1/orders"
check "admin with a wrong Bearer" "401" -H 'Authorization: Bearer admin' "$H/admin/orders"

echo "$n probes, $bad unexpected"
[ "$bad" = 0 ]
