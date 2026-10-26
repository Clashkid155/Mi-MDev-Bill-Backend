# Putting the backend on a server

This is the whole path from nothing to a running, protected, backed up backend. The plan:

* one small **Vultr** server in Johannesburg,
* **Cloudflare** in front of it (your domain is already there): it absorbs attacks and hides the server,
* **Caddy** on the server for HTTPS, the program as a **systemd** service,
* **Litestream** copying every database change to Cloudflare R2 within about a second,
* encrypted hourly snapshots in R2 as a second, independent backup.

Replace `api.example.com` with your own name everywhere.

## What runs where

```
customer app / Kora / ABS
          |  HTTPS
      Cloudflare          (absorbs floods, hides the server's address, basic firewall)
          |  HTTPS, only from Cloudflare's addresses (the firewall enforces it)
        Caddy             (the only thing listening on the internet side)
          |  127.0.0.1:8080
   bills-backend          (systemd service, user "bills", sees only its own folders)
          |
   /var/lib/bills/bills.db            the database
      |            |
      |            +-- /var/backups/bills/   hourly checked snapshots made by the program
      |                       |  cron, hourly
      |                  offsite-backup.sh  --> R2 bucket "bills-backups"   (encrypted with age)
      |
   litestream  ------------------------------> R2 bucket "bills-litestream" (every change, ~1 second)
```

## 1. Create the server on Vultr

In the Vultr dashboard choose **Deploy new server**, then:

| Setting | Choose |
|---|---|
| Type | Cloud Compute, **High Performance, AMD** |
| Location | **Johannesburg** (about 110 ms from Nigeria; Germany is about 245 ms) |
| Plan | **1 vCPU, 1 GB RAM, 25 GB NVMe, 2 TB bandwidth, $6 a month** (plan id `vhp-1c-1gb-amd`) |
| Image | **Ubuntu 24.04 LTS x64** |
| SSH key | Add yours (Account, SSH Keys). Do not use a password. |
| IPv4 | On (ABS and the webhooks need it) |
| DDoS protection add-on | Off for now. Cloudflare covers it. |
| Automatic backups | Optional. Ours cover you, so skip it unless you want one more layer. |
| Server name | `bills` |

Why these: the program and Caddy use well under 300 MB, so 1 GB is plenty. NVMe helps SQLite, which waits
on the disk for every write. The cheaper alternative is Regular Performance, 1 vCPU, 1 GB, 25 GB SSD,
1 TB bandwidth, $5 (`vc2-1c-1gb`); it works too. Prices and plan names change, so check them on the
deploy screen. Vultr bills by the hour up to a monthly cap, so you can delete the server if you change your mind.

Note the server's IP address.

## 2. Prepare the server

In your Cloudflare DNS, add an `A` record: name `api`, the server's IP, **Proxy status: Proxied** (orange cloud).

Log in as root and run:

```bash
# updates, firewall, automatic security patches, brute force protection
apt update && apt -y upgrade
apt -y install ufw fail2ban unattended-upgrades caddy gzip curl
ufw default deny incoming && ufw default allow outgoing
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp   # 80 and 443 are narrowed to Cloudflare in step 6
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

## 3. ABS, and your changing home address

The ABS dashboard lets you restrict the API key to certain IP addresses. Do it, with **the server's address
only**: then a stolen key is useless from anywhere else.

Your home and phone addresses change, so do not put them in. Instead, while developing on your laptop,
send the program's outgoing calls through the server, so ABS always sees the server's address.

On the server, once, make a login that can do nothing except carry traffic (put your public key in the
place shown):

```bash
useradd --create-home --shell /usr/sbin/nologin tunnel
install -d -o tunnel -g tunnel -m 700 /home/tunnel/.ssh
echo 'restrict,port-forwarding ssh-ed25519 AAAA...your public key... you@laptop' > /home/tunnel/.ssh/authorized_keys
chown tunnel:tunnel /home/tunnel/.ssh/authorized_keys && chmod 600 /home/tunnel/.ssh/authorized_keys
```

On your laptop, whenever you test against live ABS:

```bash
ssh -N -D 1080 tunnel@SERVER_IP            # leave this running
HTTPS_PROXY=socks5://127.0.0.1:1080 go run .
```

Go's HTTP client follows `HTTPS_PROXY`, so ABS, Kora and the email service are all reached through the
server. Calls to `localhost` (your own app) are not proxied. If ABS says the address is not allowed, check
that the tunnel is running and the server's IP is the one in the dashboard.

Kora: look for the same IP setting in its dashboard.

## 4. The program

Build it on your own computer (the Go version is pinned in `go.mod`, so everyone builds with the
patched one) and copy it over:

```bash
GOOS=linux GOARCH=amd64 go build -o bills-backend .
scp bills-backend root@api.example.com:/opt/bills/bills-backend
scp deploy/bills.service root@api.example.com:/etc/systemd/system/bills.service
```

Use `GOARCH=arm64` only if you chose an ARM server.

## 5. Its settings

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

Start it after the backups are set up (step 7), so Litestream is running from the first order.

In production the server refuses to start if `ADMIN_TOKEN`, `ABS_API_KEY`, `ABS_WEBHOOK_SECRET` or the
payment provider's keys are missing, so a half finished setup tells you at once.

## 6. HTTPS and Cloudflare

**Certificate.** In Cloudflare open SSL/TLS, Origin Server, **Create Certificate** (free, up to 15 years).
Save the certificate as `/etc/caddy/origin.pem` and the private key as `/etc/caddy/origin.key` on the server
(Cloudflare shows the key once). Then:

```bash
chown root:caddy /etc/caddy/origin.pem /etc/caddy/origin.key && chmod 640 /etc/caddy/origin.key
cp deploy/Caddyfile /etc/caddy/Caddyfile      # edit the name first
mkdir -p /var/log/caddy && chown caddy: /var/log/caddy
systemctl reload caddy
```

**Cloudflare settings** for the `api` name:

* SSL/TLS mode: **Full (strict)**.
* **Bot Fight Mode: off.** The Flutter app is not a browser, and Cloudflare can mistake it, and the Kora and
  ABS webhooks, for bots and block them. The free plan cannot skip Bot Fight Mode for chosen paths, so keep it off.
* Do not turn on "I'm Under Attack" mode except in a real emergency: it blocks the app and the webhooks too.
* Leave DDoS protection and the free managed firewall rules on (they are on by default).
* Add one rate limiting rule if the plan offers it (for example `/v1/orders`, 60 requests a minute per address).
  The program already limits by address, so this is a second layer.
* Notifications: turn on the email notifications for DDoS attacks (Notifications, Add).

**Close the server to everyone but Cloudflare.** Run this on the server (it downloads Cloudflare's list of
addresses, closes ports 80 and 443 to everyone, and opens 443 to those addresses only):

```bash
scp deploy/cloudflare-firewall.sh root@api.example.com:/opt/bills/ && ssh root@api.example.com /opt/bills/cloudflare-firewall.sh
```

Try `DRY_RUN=1` first to see what it would do. Port 22 (SSH) is untouched. Run it again if Cloudflare announces
a change to its addresses.

After that, check `https://api.example.com/healthz` works, and that `https://SERVER_IP/healthz` does not answer.

Then, in the Kora dashboard, set the webhook URL to `https://api.example.com/webhooks/kora`. Point the
Flutter app at `https://api.example.com`. If the webhooks stop arriving, look in Cloudflare under Security,
Events for blocked requests from Kora's or ABS's addresses.

**Know when it is down.** Make a free monitor (UptimeRobot, or a second healthchecks.io check) that
requests `https://api.example.com/healthz` every 5 minutes and emails you when it fails.

## 7. Backups

There are three layers, each answering a different disaster.

| Layer | Loses at most | Protects against | Where |
|---|---|---|---|
| Litestream | about 1 second | the server dying | R2 bucket `bills-litestream` |
| Hourly snapshots, copied off, encrypted | 1 hour | Litestream itself going wrong; longer history | R2 bucket `bills-backups` |
| Hourly snapshots on the server | 1 hour | a bad deploy, a deleted row | `/var/backups/bills` |

The program writes the on-server snapshots itself (every hour, checked, the newest 48 plus one a day for
30 days, and an email to `ADMIN_ALERT_EMAIL` if one fails).

Litestream 0.5 cannot encrypt, so the Litestream bucket is readable by anyone holding its token (and by
Cloudflare). That is why its token covers only that bucket and why the encrypted snapshots exist as well.

Cloudflare may ask you to add a payment method before it switches R2 on; the free allowance (10 GB of
storage, no charge for downloads, a million writes a month) is far more than this database needs. Check the
numbers in the R2 dashboard during the first week.

### One time setup

**A. Two buckets and two tokens.** In Cloudflare R2 create the buckets `bills-litestream` and `bills-backups`.
For each, create an API token with *Object Read & Write* limited to that one bucket and keep its access key
id and secret. Note your Cloudflare account id (shown on the R2 page).

**B. The encryption key** for the snapshots. On your own computer, make it and store the **private** file in
your password manager and one more safe place. Without it those backups cannot be opened, and the server never has it.

```bash
age-keygen -o bills-age.txt     # prints the public key: age1...
```

**C. Litestream** (on the server):

```bash
cd /tmp
curl -fsSLO https://github.com/benbjohnson/litestream/releases/download/v0.5.17/litestream-0.5.17-linux-x86_64.tar.gz
curl -fsSLO https://github.com/benbjohnson/litestream/releases/download/v0.5.17/checksums.txt
grep 'linux-x86_64.tar.gz$' checksums.txt | sha256sum -c -          # must say: OK
tar -xzf litestream-0.5.17-linux-x86_64.tar.gz litestream && install -m 755 litestream /usr/local/bin/litestream
```

Use `litestream-0.5.17-linux-arm64.tar.gz` on an ARM server. Then copy `deploy/litestream.yml` to
`/etc/litestream.yml`, `deploy/litestream.service` to `/etc/systemd/system/litestream.service`, and:

```bash
cat > /etc/bills/litestream.env <<'EOF'
CF_ACCOUNT_ID=your cloudflare account id
R2_ACCESS_KEY=access key id of the bills-litestream token
R2_SECRET_KEY=secret of the bills-litestream token
EOF
chown root:bills /etc/bills/litestream.env && chmod 640 /etc/bills/litestream.env
systemctl daemon-reload
systemctl enable --now litestream
journalctl -u litestream -f         # should settle with no errors; files appear in the bucket
```

**D. Encrypted snapshots** (on the server):

```bash
apt -y install age rclone
install -m 755 offsite-backup.sh /opt/bills/offsite-backup.sh
rclone config                    # new remote named "r2", type s3, provider Cloudflare, paste the bills-backups keys
cat > /etc/bills/offsite.env <<'EOF'
AGE_RECIPIENT=age1...your public key...
RCLONE_REMOTE=r2:bills-backups
HEALTHCHECK_URL=https://hc-ping.com/your-uuid
EOF
chmod 600 /etc/bills/offsite.env
echo '17 * * * * root set -a; . /etc/bills/offsite.env; set +a; /opt/bills/offsite-backup.sh >> /var/log/bills-offsite.log 2>&1' > /etc/cron.d/bills-offsite
```

**E. The alarm.** Make a free check at healthchecks.io set to "every hour, grace 2 hours" and paste its ping
URL into `offsite.env`. If the script ever stops succeeding it emails you, which is how you learn the backups
died.

**F. Start the program** (Litestream is already running):

```bash
chown root:bills /etc/bills/env && chmod 640 /etc/bills/env
systemctl daemon-reload
systemctl enable --now bills
journalctl -u bills -f        # should say: listening on 127.0.0.1:8080, and: saved bills-...db
```

### Test the backups, every month

A backup you have never opened is a hope, not a backup. On your own computer (once:
`brew install age rclone litestream` and `go build -o bills-backend .`):

```bash
# 1. the continuous backup (Litestream)
export CF_ACCOUNT_ID=... R2_ACCESS_KEY=... R2_SECRET_KEY=...     # a token that can read bills-litestream
deploy/litestream-restore.sh

# 2. the encrypted snapshots
AGE_KEY=~/keys/bills-age.txt RCLONE_REMOTE=r2:bills-backups deploy/restore-check.sh
```

Each one restores into a temporary file, opens it and prints, for example,
`backup ok: 412 orders, 9 expenses, 15 settings`. If either says `BAD BACKUP`, fix the backups that day.

### How to read the restore check

The restore checks print two lines that matter, for example:

```
backup ok: 412 orders, 9 expenses, 15 settings
latest order: 2026-11-03T14:52:10+01:00
```

Compare them with the live server:

1. **Orders.** Ask the live server for its total with
   `GET /admin/stats?from=2026-01-01` (send the admin token) and read `orders.total`. The backup should
   show the same number, or a slightly smaller one: the difference is the orders made since that backup
   (a few seconds for Litestream, at most about an hour for the snapshots).
2. **Latest order.** Ask for the newest order with `GET /admin/orders?limit=1` and look at its time. The
   backup's latest order should be that one (Litestream) or within about an hour of it (snapshots).
3. **Expenses and settings.** These change rarely, so they should match what you last entered. If you added
   an expense this morning and the count is one short, that is just the snapshot being older.

What is not normal: `BAD BACKUP`; a much smaller order count than the live total; a latest order days old
when you take orders every day; or the newest file in a bucket being older than a few hours. Any of those
means the backups are failing: check `journalctl -u litestream`, `/var/log/bills-offsite.log` and the
healthchecks.io page, and fix it the same day.

### Restoring for real

Use Litestream first: it loses almost nothing. On a new server do steps 1 to 3 and 7C first (so the
program, the config and the Litestream program exist), then:

```bash
systemctl stop bills litestream
mkdir -p /root/old-db && mv /var/lib/bills/bills.db* /var/lib/bills/.bills.db-litestream /root/old-db/ 2>/dev/null || true
set -a; . /etc/bills/litestream.env; set +a
litestream restore -config /etc/litestream.yml -integrity-check full -o /var/lib/bills/bills.db /var/lib/bills/bills.db
chown bills:bills /var/lib/bills/bills.db && chmod 600 /var/lib/bills/bills.db
systemctl start litestream bills
```

To go back to an earlier moment (for example just before a bad change), add
`-timestamp 2026-11-03T14:00:00Z` to the restore command; try it first with `-dry-run`.

If Litestream's copy is also unusable, restore the newest encrypted snapshot instead: `deploy/restore-check.sh`
shows how to download and decrypt it; copy the result to `/var/lib/bills/bills.db` as above (and remove the
`-wal` and `-shm` files).

After any restore, check the Kora dashboard for payments in the window you may have lost and settle those
orders by hand.

## 8. Updating

```bash
GOOS=linux GOARCH=amd64 go build -o bills-backend .
scp bills-backend root@api.example.com:/opt/bills/bills-backend.new
ssh root@api.example.com 'mv /opt/bills/bills-backend.new /opt/bills/bills-backend && systemctl restart bills'
```

Restarting waits for purchases in progress (up to 2.5 minutes) before stopping. Litestream keeps running.

## Checklist before real customers

- [ ] `curl https://api.example.com/healthz` works, and `https://SERVER_IP/healthz` does not
- [ ] `/admin/orders` answers 401 without the token and 200 with it; five wrong tokens block the address
- [ ] `/abs/catalog` answers 404 (the direct provider routes are off with real money)
- [ ] `go run ./tools/abscheck` (with ABS_API_KEY set) says ABS still matches what the backend expects; run it monthly too
- [ ] The server's IP is in the ABS dashboard (and Kora's, if it has the setting), and nothing else is
- [ ] The log shows `saved bills-...db`; `journalctl -u litestream` is quiet; the offsite script has run once
- [ ] Both restore tests worked once, and you compared the numbers
- [ ] The private age key is in two places that are not the server
- [ ] `ADMIN_ALERT_EMAIL` receives a real alert (set a low `ABS_LOW_BALANCE_NAIRA` for a moment)
- [ ] Cloudflare: Full (strict), Bot Fight Mode off, DDoS notification emails on
- [ ] An uptime monitor emails you if `/healthz` stops answering
- [ ] `ssh` with a password is refused
