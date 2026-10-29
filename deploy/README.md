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
| Image | **Ubuntu 24.04 LTS x64** (see below for the other versions on the list) |
| SSH key | Add yours (Account, SSH Keys) and tick it. Do not use a password. |
| Limited user login | Leave it **on** if the deploy screen offers it (the user is called `linuxuser`, with `sudo`). You log in as that user, never as `root`, which is safer. |
| IPv4 | On (ABS and the webhooks need it) |
| DDoS protection add-on | Off for now. Cloudflare covers it. |
| Automatic backups | Optional. Ours cover you, so skip it unless you want one more layer. |
| Server name | `bills` |

Why these: the program and Caddy use well under 300 MB, so 1 GB is plenty. NVMe helps SQLite, which waits
on the disk for every write. The cheaper alternative is Regular Performance, 1 vCPU, 1 GB, 25 GB SSD,
1 TB bandwidth, $5 (`vc2-1c-1gb`); it works too. Prices and plan names change, so check them on the
deploy screen. Vultr bills by the hour up to a monthly cap, so you can delete the server if you change your mind.

**Which Ubuntu.** Vultr lists several; take **24.04 LTS**. "LTS" means long term support: security fixes
until 2029 (the dates are from Ubuntu's published schedule; check them if it matters). The commands in this guide
are written for it. The others:

* **26.04 LTS** is the newest long term release (supported to about 2031) and is a fine choice if you want the
  longest life, but this guide has not been tried on it.
* **22.04 LTS** loses its standard support in 2027. Skip it.
* **26.10** is a short term release with only about nine months of fixes. Never use it for a server that takes money.

Note the server's IP address.

**Log in as the limited user.** With "limited user login" on, `root` cannot log in over SSH (the root password
from the dashboard will be refused). Use the user you were given, with your key, and become root with `sudo`
when a step needs it. In `~/.ssh/config` set `User linuxuser` for the `bills` entry, then:

```bash
ssh bills            # logs you in as linuxuser with your key, no password
sudo -i              # a root shell, for the setup commands below (type exit to leave it)
```

**If `ssh` asks for a password instead of logging you in**, the key was not attached to the server (it must
be ticked on the deploy screen, not only saved in your account), or you are using the wrong user name. The
Vultr web console (View Console) always works: log in there as the limited user, put your public key in
`~/.ssh/authorized_keys`, and try again.

## 2. Prepare the server

In your Cloudflare DNS, add an `A` record: name `api`, the server's IP, **Proxy status: Proxied** (orange cloud).

Log in (`ssh bills`), become root with `sudo -i`, and run:

```bash
# updates, firewall, automatic security patches, brute force protection
# (the options keep the SSH settings Vultr made instead of stopping to ask about them)
export DEBIAN_FRONTEND=noninteractive
apt update && apt -y -o Dpkg::Options::=--force-confold upgrade
apt -y -o Dpkg::Options::=--force-confold install ufw fail2ban unattended-upgrades gzip curl gnupg debian-keyring debian-archive-keyring apt-transport-https
ufw default deny incoming && ufw default allow outgoing
ufw allow 22/tcp && ufw allow 80/tcp && ufw allow 443/tcp   # 80 and 443 are narrowed to Cloudflare in step 6
ufw --force enable

# Caddy from its own repository, so it gets security fixes promptly (Ubuntu's copy is older)
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
apt update && apt -y install caddy

# SSH by key only, no passwords. A file in sshd_config.d is read first and the first value wins, so this
# also beats the "PasswordAuthentication yes" that some cloud images put in their own file there.
cat > /etc/ssh/sshd_config.d/00-bills.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
EOF
sshd -t && systemctl restart ssh
sshd -T | grep -E '^(passwordauthentication|kbdinteractiveauthentication|permitrootlogin)'   # must say: no, no, no

# the service user and its folders
useradd --system --home /var/lib/bills --shell /usr/sbin/nologin bills
install -d -o bills -g bills -m 700 /var/lib/bills /var/backups/bills
install -d -o root -g bills -m 750 /etc/bills /opt/bills
```

If a screen asks about `sshd_config` anyway, choose "keep the local version currently installed": Vultr changed
that file to set up your login, and our SSH settings live in a separate file that applies either way. If the
upgrade says a restart is needed (`/var/run/reboot-required`), run `reboot` and log in again.

Open a second terminal and confirm `ssh bills` still works before closing the first.

## 3. ABS and the server's address

Tested on 2026-10-27 with `abscheck`, from a laptop whose address was NOT in ABS's whitelist:

* **The Direct API is not restricted by the whitelist.** Buying, the catalog, the wallet balance and meter
  checks (everything the backend does to spend money) worked from an address that was not listed. A stolen API key
  can therefore be used from anywhere. The protection is keeping the key secret, and the size of the wallet
  (see below). Because of this you can develop from your laptop with no special setup, whatever its address.
* **The Partner API is restricted.** Its calls (the order lookup, the account details) are refused unless the
  caller's address is in the whitelist, and the key must have a website saved under API settings
  (`https://absvendor.com/dashboard/apikeys.php`), or ABS answers `SITE_URL_REQUIRED`.

So in the ABS dashboard add **the server's address** to the IP whitelist and keep a website saved. Your laptop's
address does not need to be there.

What protects the Direct key, since the whitelist cannot:

* The key lives only in `/etc/bills/env` (readable by root and the `bills` user only). Never put it in the
  repository, a chat, or a shared run configuration.
* **Keep the ABS wallet small.** The most anyone can spend with a stolen key is what the wallet holds.
  Top it up often, in small amounts; the low-balance alert (`ABS_LOW_BALANCE_NAIRA`) tells you when to.
* If you ever suspect the key, replace it on that same ABS page and update `/etc/bills/env`.
* Ask ABS whether the Direct API can be restricted by address or capped per day, as the Partner API can.

## 4. The program

The first time, put the service file on the server (the program itself is installed by the deploy script, below):

```bash
scp deploy/bills.service bills:/tmp/ && ssh bills 'sudo install -m 644 /tmp/bills.service /etc/systemd/system/bills.service && sudo systemctl daemon-reload'
```

Then, from your laptop, in the project folder:

```bash
deploy/deploy.sh
```

It checks the code (vet, every test, known vulnerabilities), builds the program with the Go version pinned in
`go.mod`, copies it to the server, checks the copy is identical, swaps it in (keeping the old one as
`bills-backend.prev`), restarts the service and checks the server answers, putting the old version back if it
does not. **Every change you make to the code reaches the server this way**, so run it after each change you
want live. It never touches your settings (`/etc/bills/env`) or the database. Use `DRY_RUN=1 deploy/deploy.sh` to
see it build without touching the server.

Use `TARGET_ARCH=arm64` only if you chose an ARM server.

## 5. Its settings

The commands from here on that touch `/etc`, `/opt` or `systemctl` need root: run them in a `sudo -i` shell on the
server (`ssh bills`, then `sudo -i`).

Create `/etc/bills/env` (owner `root`, group `bills`, mode `640`) with the settings from `.env.example`.
These are the ones that matter for a live server:

```
APP_ENV=production
ABS_SANDBOX=false
HTTP_ADDR=127.0.0.1:8080
TRUST_PROXY=true
DATABASE_PATH=/var/lib/bills/bills.db
BACKUP_DIR=/var/backups/bills
ADMIN_TOKEN=<a long random string: openssl rand -hex 32>
ABS_API_KEY=...
PAYMENT_PROVIDER=kora
KORA_SECRET_KEY=...
ZEPTOMAIL_TOKEN=...
MAIL_FROM_EMAIL=...
MAIL_SANDBOX=false
ADMIN_ALERT_EMAIL=you@example.com
```

Start it after the backups are set up (step 7), so Litestream is running from the first order.

In production the server refuses to start if `ADMIN_TOKEN`, `ABS_API_KEY` or the payment provider's keys are
missing, or if `ABS_SANDBOX` is not `false` (with ABS in sandbox, customers would pay and receive nothing), so a half
finished setup tells you at once. There is no ABS webhook secret to set: nothing uses ABS webhooks.

### Admin tokens for the dashboard app

`ADMIN_TOKEN` is the **owner** token: keep it for yourself, in your password manager. Give every other person or
device its own named token, so you can tell them apart in the audit log and cancel one without touching the rest:

```bash
curl -s -X POST https://api.dizztech.com/admin/tokens -H "Authorization: Bearer OWNER_TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"dizz phone","role":"admin"}'
```

The answer holds the token **once** (`mdev_...`); the server keeps only a hash, so it cannot show it again. Put it in
the phone's secure storage (Keychain or Keystore), never in the app's code.

| Role | May | May not |
|---|---|---|
| `owner` (the `ADMIN_TOKEN`) | everything, including making and revoking tokens | |
| `admin` | read and change orders, refunds, expenses, settings; read the audit log | manage tokens |
| `viewer` | read orders, stats, balances, expenses, settings | change anything; read the audit log |

* `GET /admin/whoami` says which token is in use and its role (the app can call it after sign in).
* `GET /admin/tokens` lists them (name, role, last 4 characters, last used and from where, revoked or not).
* `DELETE /admin/tokens/{id}` revokes one at once. A lost phone: revoke its token and the rest carry on.
* `GET /admin/audit` shows every change: who, from which address, which route, the order id, the result, and what was
  sent (anything called token, secret, password or key is blanked). Reads are not logged. A viewer being refused and an
  address being locked out for guessing a token are logged too.
* The owner is emailed whenever a token is made or revoked, so a token made by someone who got hold of yours is noticed.

Wrong-token guesses still count toward the lockout (5 in a row blocks that address for 15 minutes), for named tokens too.

**Who holds what.** The owner token lives in your password manager and nowhere else; never put it in an app. The dashboard
on your phone gets an `admin` token, and anyone who only needs to look gets a `viewer` token. Then a stolen phone cannot
make or revoke tokens.

**Setting it up the first time** (after the deploy that adds these endpoints; production answers 404 for them before that):

1. Make a token with the owner token (`POST /admin/tokens`, or "Make a token (owner)" in Postman).
2. Copy the `token` from the answer straight into the app. It is shown once and cannot be recovered.
3. Call `GET /admin/whoami` with it to check the name and role are what you expect.

**A lost or stolen phone.**

1. `GET /admin/tokens` with the owner token and find the phone's token by its name and last 4 characters.
2. `DELETE /admin/tokens/{id}`. It stops working at once; every other token carries on.
3. Check `GET /admin/audit?actor=<that name>` for anything it did before you revoked it.
4. Make a new token for the replacement phone.

**Changing the owner token** (for example if it may have leaked): put a new long random value in `ADMIN_TOKEN` in
`/etc/bills/env` and run `sudo systemctl restart bills`. The old owner token stops working at once. Named tokens are
stored in the database and are **not** affected; to cancel those too, revoke each one.

**After restoring a backup** (Litestream or a snapshot): run `GET /admin/tokens` and check the list. A restore brings the
tokens back as they were at that moment, so a token you revoked after the backup was taken **works again**, and one made
after it is gone. Revoke the first kind again and make the second kind again.

**Where it is stored.** Named tokens (as a one way scramble, never the token), the audit log and the events are in the
database, so they are in the Litestream copy and the snapshots like everything else.

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

If the reload fails with "permission denied" on `/var/log/caddy/bills-access.log`, a check you ran as root
created that file owned by root. Delete it (`rm /var/log/caddy/bills-access.log`) and reload again, and check
configs as the Caddy user from now on: `sudo -u caddy caddy validate --config /etc/caddy/Caddyfile`.

**Cloudflare settings** for the `api` name:

* SSL/TLS mode: **Full (strict)**.
* **Bot Fight Mode: off.** (In the current dashboard it is under Security, Settings, "Bot traffic"; if you cannot
  find it, test instead: request your API with `curl` and a Python user agent, and if Cloudflare answers with a
  challenge page or a `cf-mitigated` header rather than your server's answer, it is on.) The Flutter app is not a browser, and Cloudflare can mistake it, and the Kora and
  ABS webhooks, for bots and block them. The free plan cannot skip Bot Fight Mode for chosen paths, so keep it off.
* Do not turn on "I'm Under Attack" mode except in a real emergency: it blocks the app and the webhooks too.
* Leave DDoS protection and the free managed firewall rules on (they are on by default).
* Add one rate limiting rule if the plan offers it (for example `/v1/orders`, 60 requests a minute per address).
  The program already limits by address, so this is a second layer.
* Notifications: turn on the email notifications for DDoS attacks (Notifications, Add).

**Close the server to everyone but Cloudflare.** Run this on the server (it downloads Cloudflare's list of
addresses, closes ports 80 and 443 to everyone, and opens 443 to those addresses only):

```bash
scp deploy/cloudflare-firewall.sh bills:/tmp/ && ssh bills 'sudo bash /tmp/cloudflare-firewall.sh'
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

**C. The tools.** Caddy, Litestream, rclone and age are built on your laptop from their released source, with
the newest Go and the newest versions of the libraries they use, and scanned for known vulnerabilities. This
matters: the downloads and packages are built weeks earlier with older libraries, and when this was set up
the packaged Caddy and Litestream carried 16 and 26 known flaws, while these builds carry none (one
unfixable "unmaintained library" notice, which every build has).

```bash
deploy/build-tools.sh
```

Then put the Litestream files on the server:

```bash
scp deploy/litestream.yml deploy/litestream.service deploy/offsite-backup.sh bills:/tmp/
ssh bills 'sudo install -m 644 /tmp/litestream.yml /etc/litestream.yml && sudo install -m 644 /tmp/litestream.service /etc/systemd/system/litestream.service && sudo install -m 755 /tmp/offsite-backup.sh /opt/bills/offsite-backup.sh && sudo systemctl daemon-reload'
```

and install the tools (this also removes Ubuntu's older `age` and `rclone`, so there is one of each):

```bash
deploy/install-tools.sh
```

On the server, create Litestream's settings and start it:

```bash
cat > /etc/bills/litestream.env <<'EOF'
CF_ACCOUNT_ID=your cloudflare account id
R2_ACCESS_KEY=access key id of the bills-litestream token
R2_SECRET_KEY=secret of the bills-litestream token
EOF
chown root:bills /etc/bills/litestream.env && chmod 640 /etc/bills/litestream.env
systemctl enable --now litestream
journalctl -u litestream -f         # should settle with no errors; files appear in the bucket
```

Check the secret is exactly 64 characters with no extra letter at the end; a stray character makes R2 refuse it.

**D. Encrypted snapshots** (on the server):

```bash
mkdir -p /root/.config/rclone
nano /root/.config/rclone/rclone.conf   # paste the block below, with the bills-backups keys
chmod 600 /root/.config/rclone/rclone.conf
```

```
[r2]
type = s3
provider = Cloudflare
access_key_id = access key id of the bills-backups token
secret_access_key = secret of the bills-backups token
region = auto
endpoint = https://your cloudflare account id.r2.cloudflarestorage.com
acl = private
no_check_bucket = true
```

(`no_check_bucket` is needed because the token covers one bucket only.) Then the settings and the hourly job:

```bash
cat > /etc/bills/offsite.env <<'EOF'
AGE_RECIPIENT=age1...your public key...
RCLONE_REMOTE=r2:bills-backups
HEALTHCHECK_URL=https://hc-ping.com/your-uuid
EOF
chmod 600 /etc/bills/offsite.env
printf '%s\n' 'PATH=/usr/local/bin:/usr/bin:/bin' \
  "17 * * * * root set -a; . /etc/bills/offsite.env; set +a; /opt/bills/offsite-backup.sh >> /var/log/bills-offsite.log 2>&1" \
  > /etc/cron.d/bills-offsite
```

The `PATH` line matters: cron's own default leaves out `/usr/local/bin`, where the tools live. Test it once by hand
(`set -a; . /etc/bills/offsite.env; set +a; /opt/bills/offsite-backup.sh`) and check a file appears in the bucket.

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

## 8. Reading the logs

The program writes to the system journal. On the server, or from your laptop with `ssh bills '...'`:

```bash
journalctl -u bills -f                          # live, newest at the bottom (Ctrl+C to stop)
journalctl -u bills -n 200 --no-pager           # the last 200 lines
journalctl -u bills --since "1 hour ago"        # a time window ("today", "2026-11-03 14:00" also work)
journalctl -u bills -p warning --since today    # only warnings and errors
journalctl -u bills | grep MDEV_abc123          # everything about one order, by its id
journalctl -u litestream -n 50 --no-pager       # the continuous backup
tail -n 50 /var/log/bills-offsite.log           # the encrypted snapshot upload
tail -f /var/log/caddy/bills-access.log         # every request Caddy saw (JSON lines)
```

Lines look like `[WARN]  [GetSystemData - ABS]  HTTP 401 ...`: the level, then what and who, then the detail.
`[ERROR]` lines are the ones to read first. The program's own request lines name the route
(`GET /v1/orders/{id}`), never the order id.

Make sure the journal is kept across reboots, for six months, and cannot fill the disk (the 2 GB cap is what actually
bounds it: if the logs ever outgrow it the oldest lines go first, so look at `journalctl --disk-usage` now and then):

```bash
mkdir -p /var/log/journal /etc/systemd/journald.conf.d
printf '[Journal]\nStorage=persistent\nMaxRetentionSec=6month\nSystemMaxUse=2G\n' > /etc/systemd/journald.conf.d/bills.conf
systemctl restart systemd-journald
```

The logs hold order ids and some customer details, so treat them as private (only root can read the journal).

### Events: the readable version, for the admin app

The journal is the raw record for engineers. The program also keeps **events**: a short, redacted list of what
happened, in words an owner can read, stored in the database (so it is in the backups) for `EVENT_RETENTION_DAYS`
(180 by default). It holds orders created, paid, delivered, failed, expired and refunded, a payment that arrives late,
short or for an unknown order, each start and stop of the server (with the version, so you can see when a deploy
happened), and the server's own warnings and errors. It leaves out the web request lines, and removes phone numbers,
emails, meter numbers, electricity tokens and secrets before anything is stored. The admin app reads it with
`GET /admin/events` (filter by level, kind, order or time) and `GET /admin/events/summary` (a count for a badge), for owner
and admin tokens. It does not replace the journal: when the server is down or crashing, `journalctl` is still where the real
error is.

## 9. Deploying changes and keeping everything up to date

**Every code change:** commit it if you like, then run `deploy/deploy.sh` (step 4). Changing a setting in
`/etc/bills/env` needs `sudo systemctl restart bills` on the server; many pricing, alert and mail settings can
instead be changed live through the admin API (`PUT /admin/settings/{key}`) with no restart.

**Everything else goes stale, and stale means known security holes.** The rule here is: keep every program and
library on its newest release unless there is a written reason not to. The routine:

| What | How it stays current |
|---|---|
| Ubuntu and its packages | `unattended-upgrades` installs security updates by itself. A kernel update needs a `sudo reboot` now and then; `deploy/audit.sh` says when. |
| Our Go libraries | `go get -u ./... && go mod tidy`, run the tests, then `deploy/deploy.sh` |
| The Go version | change the `toolchain` line in `go.mod` to the newest patch release; rebuild everything |
| Caddy, Litestream, rclone, age | raise the version at the top of `deploy/build-tools.sh`, run it, then `deploy/install-tools.sh` |

Once a month, and whenever there is security news, run:

```bash
deploy/audit.sh
```

It lists newer library and tool versions, scans our code for known vulnerabilities, shows the server's pending
updates, whether a restart is needed, whether every service is healthy, the backup freshness, whether the tools
on the server are exactly the ones built here, and what the live site answers to the usual attacks
(`deploy/probe.sh`, which you can also run alone). **It makes three wrong admin requests, which counts toward
the admin lockout (5 in a row); a restart of the service clears it.**

### Testing the scripts themselves

`deploy/deploy.sh` and `deploy/install-tools.sh` swap programs on a live server, so their failure handling matters
more than their happy path. `deploy/tests/run.sh` runs both against a fake server (a folder, with stand-ins for
`ssh`, `scp`, `systemctl`, `curl` and the rest) and simulates the bad days: a corrupted copy, a program or Caddy
that does not come back, Caddy rejecting its config, Litestream logging errors, a tampered build, a first install.
Run it after changing either script. It proves the logic (what is kept, rolled back and reported), not that real
systemd or Caddy behave; the live runs showed that. `shellcheck deploy/*.sh` is also clean.

## Checklist before real customers

- [ ] `curl https://api.example.com/healthz` works, and `https://SERVER_IP/healthz` does not
- [ ] `/admin/orders` answers 401 without the token and 200 with it; five wrong tokens block the address
- [ ] `/abs/catalog` answers 404 (the direct provider routes are off with real money)
- [ ] `go run ./tools/abscheck` (with ABS_API_KEY set) says ABS still matches what the backend expects; run it monthly too
- [ ] The server's IP is in the ABS whitelist and a website is saved there (the Partner lookup needs both); Kora's dashboard checked for a similar setting
- [ ] The ABS wallet holds only what you are willing to lose (the Direct API key cannot be restricted by address)
- [ ] The log shows `saved bills-...db`; `journalctl -u litestream` is quiet; the offsite script has run once
- [ ] Both restore tests worked once, and you compared the numbers
- [ ] The private age key is in two places that are not the server
- [ ] `ADMIN_ALERT_EMAIL` receives a real alert (set a low `ABS_LOW_BALANCE_NAIRA` for a moment)
- [ ] Cloudflare: Full (strict), Bot Fight Mode off, DDoS notification emails on
- [ ] An uptime monitor emails you if `/healthz` stops answering
- [ ] `ssh` with a password is refused
- [ ] `deploy/audit.sh` shows nothing to update and no unexpected answers from the live site
