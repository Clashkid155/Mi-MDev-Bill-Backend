# Putting the backend on a server

This is the whole path from a blank server to a running, backed up backend. It assumes one small
Linux VPS (Ubuntu 24.04 LTS), a domain name you control, and a Cloudflare account for backups.
The backend is one program plus one database file, so there is nothing else to install on the server.

Replace `api.example.com` with your own name everywhere.

## What runs where

```
customer app / Kora / ABS
          |  HTTPS
        Caddy            (gets the certificate, the only thing open to the internet)
          |  127.0.0.1:8080
   bills-backend         (systemd service, user "bills", sees only its own folders)
          |
   /var/lib/bills/bills.db          the database
   /var/backups/bills/              hourly snapshots made by the program
          |  cron, hourly
   offsite-backup.sh  -->  Cloudflare R2   (encrypted before it leaves the server)
```

## 1. The server

1. Create an Ubuntu 24.04 server, add your SSH key during creation, and note its IP.
2. In your DNS, add an `A` record: `api.example.com` pointing at that IP.
3. Log in as root and run:

```bash
# updates, firewall, automatic security patches, brute force protection
apt update && apt -y upgrade
apt -y install ufw fail2ban unattended-upgrades caddy gzip curl
ufw default deny incoming && ufw default allow outgoing
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp
ufw --force enable

# SSH by key only, no root password logins
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
systemctl reload ssh

# the service user and its folders
useradd --system --home /var/lib/bills --shell /usr/sbin/nologin bills
install -d -o bills -g bills -m 700 /var/lib/bills /var/backups/bills
install -d -o root -g bills -m 750 /etc/bills /opt/bills
```

Open a second terminal and confirm you can still log in before closing the first.

## 2. The program

Build it on your own computer (the Go version is pinned in `go.mod`, so everyone builds with the
patched one) and copy it over:

```bash
GOOS=linux GOARCH=amd64 go build -o bills-backend .
scp bills-backend root@api.example.com:/opt/bills/bills-backend
scp deploy/bills.service root@api.example.com:/etc/systemd/system/bills.service
```

Use `GOARCH=arm64` if you chose an ARM server.

## 3. Its settings

Create `/etc/bills/env` (owner `root`, group `bills`, mode `640`) with the settings from `.env.example`.
These are the ones that matter for a live server:

```
APP_ENV=production
HTTP_ADDR=127.0.0.1:8080
TRUST_PROXY=true
DATABASE_PATH=/var/lib/bills/bills.db
BACKUP_DIR=/var/backups/bills
ADMIN_TOKEN=<a long random string: openssl rand -hex 32>
ABS_API_KEY=...
ABS_WEBHOOK_SECRET=...
PAYMENT_PROVIDER=kora
KORA_SECRET_KEY=...
ZEPTOMAIL_TOKEN=...
MAIL_FROM_EMAIL=...
MAIL_SANDBOX=false
ADMIN_ALERT_EMAIL=you@example.com
```

```bash
chown root:bills /etc/bills/env && chmod 640 /etc/bills/env
systemctl daemon-reload
systemctl enable --now bills
journalctl -u bills -f        # should say: listening on 127.0.0.1:8080, and: saved bills-...db
```

In production the server refuses to start if `ADMIN_TOKEN`, `ABS_API_KEY`, `ABS_WEBHOOK_SECRET` or the
payment provider's keys are missing, so a half finished setup tells you at once.

## 4. HTTPS

```bash
cp deploy/Caddyfile /etc/caddy/Caddyfile      # edit the name first
mkdir -p /var/log/caddy && chown caddy: /var/log/caddy
systemctl reload caddy
curl https://api.example.com/healthz          # {"status":"ok",...}
```

Then, in the Kora dashboard, set the webhook URL to `https://api.example.com/webhooks/kora`. Point the
Flutter app at `https://api.example.com`.

## 5. Backups

There are two layers.

**On the server.** The program writes a snapshot of the database to `BACKUP_DIR` every hour, checks the
copy is intact, keeps the newest 48 and one per day for 30 days, and emails `ADMIN_ALERT_EMAIL` if a snapshot
ever fails. This protects against a bad deploy or a deleted row.

**Off the server.** A disk failure or a closed account takes the server and its snapshots together, so
`offsite-backup.sh` copies the newest snapshot to Cloudflare R2 every hour, encrypted. R2's free plan
(10 GB of storage, and no charge for downloading) is far more than this database needs.

One time setup:

**A. Cloudflare.** Create an R2 bucket (for example `bills-backups`) and an API token that can edit only that
bucket. Keep the access key id and secret.

**B. The encryption key.** On your own computer, make it and store the **private** file in your password
manager and one more safe place. Without it the backups cannot be opened, and the server never has it.

```bash
age-keygen -o bills-age.txt     # prints the public key: age1...
```

**C. The server.**

```bash
apt -y install age rclone
install -m 755 offsite-backup.sh /opt/bills/offsite-backup.sh
rclone config                    # new remote named "r2", type s3, provider Cloudflare, paste the keys
cat > /etc/bills/offsite.env <<'EOF'
AGE_RECIPIENT=age1...your public key...
RCLONE_REMOTE=r2:bills-backups
HEALTHCHECK_URL=https://hc-ping.com/your-uuid
EOF
chmod 600 /etc/bills/offsite.env
echo '17 * * * * root set -a; . /etc/bills/offsite.env; set +a; /opt/bills/offsite-backup.sh >> /var/log/bills-offsite.log 2>&1' > /etc/cron.d/bills-offsite
```

**D. The alarm.** Make a free check at healthchecks.io set to "every hour, grace 2 hours" and paste its ping
URL into `offsite.env`. If the script ever stops succeeding it emails you, which is how you learn the backups
died.

### Test a restore, every month

A backup you have never opened is a hope, not a backup. On your own computer:

```bash
brew install age rclone
go build -o bills-backend .
AGE_KEY=~/keys/bills-age.txt RCLONE_REMOTE=r2:bills-backups deploy/restore-check.sh
```

It downloads the newest backup, decrypts it with your private key, opens it and prints, for example,
`backup ok: 412 orders, 9 expenses, 15 settings`. Check that the number looks right for the business. If it
says `BAD BACKUP`, fix the backups that day.

### How to read the restore check

`restore-check.sh` prints two lines that matter, for example:

```
backup ok: 412 orders, 9 expenses, 15 settings
latest order: 2026-11-03T14:52:10+01:00
```

Compare them with the live server:

1. **Orders.** Ask the live server for its total with
   `GET /admin/stats?from=2026-01-01` (send the admin token) and read `orders.total`. The backup should
   show the same number, or a slightly smaller one: the difference is the orders made since that snapshot,
   at most about an hour of them.
2. **Latest order.** Ask for the newest order with `GET /admin/orders?limit=1` and look at its time. The
   backup's latest order should be within about an hour of it.
3. **Expenses and settings.** These change rarely, so they should match what you last entered. If you added
   an expense this morning and the count is one short, that is just the snapshot being older.

What is not normal: `BAD BACKUP`; a much smaller order count than the live total; a latest order days old
when you take orders every day; or the newest file in the bucket being older than a few hours. Any of those
means the backups are failing: check `/var/log/bills-offsite.log` on the server and the healthchecks.io page,
and fix it the same day.

### Restoring for real

```bash
systemctl stop bills
# decrypt a backup (see restore-check.sh), then:
cp restored.db /var/lib/bills/bills.db && chown bills:bills /var/lib/bills/bills.db && chmod 600 /var/lib/bills/bills.db
rm -f /var/lib/bills/bills.db-wal /var/lib/bills/bills.db-shm
systemctl start bills
```

Orders made since the backup are not in it. After restoring, check the Kora dashboard for payments in that
window and settle those orders by hand. At most one hour of orders is exposed.

## 6. Updating

```bash
go build ... -o bills-backend .   # as in step 2
scp bills-backend root@api.example.com:/opt/bills/bills-backend.new
ssh root@api.example.com 'mv /opt/bills/bills-backend.new /opt/bills/bills-backend && systemctl restart bills'
```

Restarting waits for purchases in progress (up to 2.5 minutes) before stopping.

## Checklist before real customers

- [ ] `curl https://api.example.com/healthz` works, and `http://` redirects to `https://`
- [ ] `/admin/orders` answers 401 without the token and 200 with it
- [ ] `/abs/catalog` answers 404 (the direct provider routes are off with real money)
- [ ] The log shows `saved bills-...db` and the offsite script has run once (check healthchecks.io)
- [ ] `restore-check.sh` worked once
- [ ] The private age key is in two places that are not the server
- [ ] `ADMIN_ALERT_EMAIL` receives a real alert (set a low `ABS_LOW_BALANCE_NAIRA` for a moment)
- [ ] `ssh` with a password is refused
