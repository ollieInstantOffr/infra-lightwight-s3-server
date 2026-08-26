#!/usr/bin/env bash
#
# Pail setup — asks what it cannot work out, writes .env, and brings the stack
# up.
#
#   ./setup.sh              configure and start
#   ./setup.sh --configure  write .env only, do not start
#   ./setup.sh --start      start using the existing .env
#
# Secrets are generated rather than asked for. Pasting `openssl rand` output
# into a prompt is a step that can only go wrong, and the operator gains
# nothing by choosing those bytes themselves.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

readonly ENV_FILE=".env"
readonly COMPOSE_FILE="docker-compose.yml"

# ─── Presentation ─────────────────────────────────────────────────────────────

# Colour only when attached to a terminal, so redirecting to a file or piping
# into a log does not fill it with escape sequences.
if [[ -t 1 ]] && [[ -z "${NO_COLOR:-}" ]]; then
  BOLD=$'\033[1m'; DIM=$'\033[2m'; RESET=$'\033[0m'
  GREEN=$'\033[32m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; CYAN=$'\033[36m'
else
  BOLD=""; DIM=""; RESET=""; GREEN=""; YELLOW=""; RED=""; CYAN=""
fi

heading() { printf '\n%s%s%s\n' "$BOLD" "$1" "$RESET"; }
info()    { printf '  %s\n' "$1"; }
muted()   { printf '  %s%s%s\n' "$DIM" "$1" "$RESET"; }
good()    { printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$1"; }
warn()    { printf '  %s!%s %s\n' "$YELLOW" "$RESET" "$1"; }
fail()    { printf '\n  %serror%s %s\n\n' "$RED" "$RESET" "$1" >&2; exit 1; }

# ─── Input ────────────────────────────────────────────────────────────────────

# ask <variable> <question> [default] — reads a line, falling back to default.
ask() {
  local __var=$1 question=$2 default=${3:-} reply
  local suffix=""
  [[ -n "$default" ]] && suffix=" ${DIM}[$default]${RESET}"

  printf '  %s%s: ' "$question" "$suffix"
  IFS= read -r reply || fail "input ended unexpectedly; run this from a terminal"
  [[ -z "$reply" ]] && reply=$default
  printf -v "$__var" '%s' "$reply"
}

# ask_secret <variable> <question> — reads without echoing.
ask_secret() {
  local __var=$1 question=$2 reply
  printf '  %s: ' "$question"
  IFS= read -rs reply || fail "input ended unexpectedly"
  printf '\n'
  printf -v "$__var" '%s' "$reply"
}

# confirm <question> [Y|N] — returns 0 for yes.
confirm() {
  local question=$1 default=${2:-Y} reply prompt
  if [[ $default == Y ]]; then prompt="Y/n"; else prompt="y/N"; fi

  while true; do
    printf '  %s %s[%s]%s ' "$question" "$DIM" "$prompt" "$RESET"
    IFS= read -r reply || fail "input ended unexpectedly"
    reply=${reply:-$default}
    # tr rather than ${reply,,}: case conversion is a bash 4 feature, and macOS
    # still ships bash 3.2 as /bin/bash.
    case "$(printf '%s' "$reply" | tr '[:upper:]' '[:lower:]')" in
      y|yes) return 0 ;;
      n|no)  return 1 ;;
      *) warn "Please answer y or n." ;;
    esac
  done
}

# choose <variable> <question> <option>... — numbered menu.
choose() {
  local __var=$1 question=$2; shift 2
  local options=("$@") reply index

  printf '  %s\n' "$question"
  for index in "${!options[@]}"; do
    printf '    %s%d%s) %s\n' "$CYAN" "$((index + 1))" "$RESET" "${options[$index]%%|*}"
    [[ ${options[$index]} == *"|"* ]] && printf '       %s%s%s\n' "$DIM" "${options[$index]#*|}" "$RESET"
  done

  while true; do
    printf '  Choice %s[1]%s: ' "$DIM" "$RESET"
    IFS= read -r reply || fail "input ended unexpectedly"
    reply=${reply:-1}
    if [[ $reply =~ ^[0-9]+$ ]] && (( reply >= 1 && reply <= ${#options[@]} )); then
      printf -v "$__var" '%s' "$reply"
      return 0
    fi
    warn "Enter a number between 1 and ${#options[@]}."
  done
}

# ─── Validation ───────────────────────────────────────────────────────────────

valid_email() { [[ $1 =~ ^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$ ]]; }
valid_url()   { [[ $1 =~ ^https?://[^[:space:]/]+ ]]; }

# ask_until <variable> <validator> <message> <question> [default]
ask_until() {
  local __var=$1 validator=$2 message=$3 question=$4 default=${5:-} value
  while true; do
    ask value "$question" "$default"
    if "$validator" "$value"; then
      printf -v "$__var" '%s' "$value"
      return 0
    fi
    warn "$message"
  done
}

# ─── Secrets ──────────────────────────────────────────────────────────────────

# generate_secret — 32 random bytes, base64. openssl where available, the
# kernel otherwise, so this works on a machine with no openssl installed.
generate_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 32 | tr -d '\n'
  else
    head -c 32 /dev/urandom | base64 | tr -d '\n'
  fi
}

# generate_url_safe_secret — hex, for anything that ends up inside a URL.
#
# The database password is interpolated into DATABASE_URL by the compose file.
# Base64 output contains "/", "+" and "=", and a "/" in the password position
# makes the URL unparseable — the driver reads the rest as a port and fails
# with "invalid port after host", which points nowhere near the real cause.
# Hex has no such characters and needs no escaping.
generate_url_safe_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 24 | tr -d '\n'
  else
    head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

# read_env <key> — reads a value out of the existing .env, if any.
read_env() {
  [[ -f $ENV_FILE ]] || return 1
  local line
  line=$(grep -E "^$1=" "$ENV_FILE" | tail -1) || return 1
  printf '%s' "${line#*=}"
}

# ─── Prerequisites ────────────────────────────────────────────────────────────

COMPOSE=()

check_prerequisites() {
  heading "Checking prerequisites"

  command -v docker >/dev/null 2>&1 ||
    fail "Docker is not installed. See https://docs.docker.com/get-docker/"

  docker info >/dev/null 2>&1 ||
    fail "Docker is installed but not running. Start Docker Desktop, or the docker service, and try again."

  # Compose v2 is a docker subcommand; v1 was a separate binary. Both are still
  # in the wild, so whichever exists is used.
  if docker compose version >/dev/null 2>&1; then
    COMPOSE=(docker compose)
  elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE=(docker-compose)
  else
    fail "Docker Compose is not available. Install Docker Desktop, or the docker-compose-plugin package."
  fi

  [[ -f $COMPOSE_FILE ]] ||
    fail "$COMPOSE_FILE is missing. Run this from the repository root."

  good "Docker $(docker version --format '{{.Server.Version}}' 2>/dev/null || echo 'running')"
  good "Compose available as: ${COMPOSE[*]}"
}

# ─── Configuration ────────────────────────────────────────────────────────────

configure() {
  heading "Pail setup"
  muted "S3-compatible object storage on your own hardware."
  muted "Press enter to accept the value in brackets."

  # Existing configuration is reused where it is safe to, and never silently
  # overwritten.
  local reuse_secrets=false
  if [[ -f $ENV_FILE ]]; then
    heading "An existing $ENV_FILE was found"
    if ! confirm "Reconfigure it?" N; then
      info "Keeping the existing configuration."
      return 1
    fi
    reuse_secrets=true
  fi

  # ── Where this will run ────────────────────────────────────────────────────
  heading "1. Where will this run?"

  local placement
  choose placement "How will people reach this server?" \
    "On this machine only|Console and S3 API on localhost. Good for trying it out." \
    "Behind a reverse proxy|You have domains pointing at this host, with TLS handled by nginx, Caddy or similar."

  local public_s3 public_console s3_domain="" bind_address
  if [[ $placement == 1 ]]; then
    public_s3="http://localhost:8443"
    public_console="http://localhost:8444"
    bind_address="127.0.0.1"
    muted "S3 API:  $public_s3"
    muted "Console: $public_console"
  else
    muted "These must be the URLs clients actually use. SigV4 signs the hostname,"
    muted "so a mismatch here makes every S3 request fail with SignatureDoesNotMatch."
    printf '\n'
    ask_until public_s3 valid_url "Enter a full URL, such as https://s3.example.com" \
      "Public URL for the S3 API" "https://s3.example.com"
    ask_until public_console valid_url "Enter a full URL, such as https://console.example.com" \
      "Public URL for the console" "https://console.example.com"

    printf '\n'
    muted "Virtual-host style addressing lets clients use bucket.s3.example.com."
    muted "It needs a wildcard DNS record and certificate. Path style always works."
    if confirm "Enable virtual-host style addressing?" N; then
      local host=${public_s3#*://}
      ask s3_domain "Base domain for buckets" "${host%%/*}"
    fi

    printf '\n'
    muted "The ports are published on the host for the proxy to reach."
    muted "Loopback is right when the proxy runs on this machine."
    ask bind_address "Bind the ports to which address" "127.0.0.1"
  fi

  # ── Administrator ──────────────────────────────────────────────────────────
  heading "2. Who administers it?"
  muted "This address can always sign in, and invites everyone else."
  muted "It is re-promoted on every start, so it is also the way back in."
  printf '\n'
  ask_until admin_email valid_email "That does not look like an email address." \
    "Administrator email" "$(read_env ADMIN_EMAIL || echo '')"

  # ── Email ──────────────────────────────────────────────────────────────────
  heading "3. Alert email"
  muted "Signing in uses a password and never needs email. This is only for"
  muted "alert notifications — without it, alerts still appear in the console,"
  muted "they just do not reach you when you are not looking at it."
  muted "It can also be configured later under Settings in the console."
  printf '\n'

  local resend_key="" resend_from=""
  if confirm "Configure Resend for alert email?" N; then
    ask_secret resend_key "Resend API key (starts with re_)"
    if [[ -n $resend_key ]]; then
      local domain=${admin_email#*@}
      ask resend_from "From address" "Pail <no-reply@$domain>"
    fi
  else
    muted "Skipped. Set it later in the console under Settings."
  fi

  # ── Region ─────────────────────────────────────────────────────────────────
  heading "4. Region"
  muted "Any value works, as long as your clients use the same one."
  printf '\n'
  ask s3_region "S3 region" "$(read_env S3_REGION || echo 'us-east-1')"

  # ── Secrets ────────────────────────────────────────────────────────────────
  heading "5. Secrets"

  local session_secret credentials_key postgres_password
  local existing_credentials_key=""
  $reuse_secrets && existing_credentials_key=$(read_env CREDENTIALS_KEY || echo "")

  if [[ -n $existing_credentials_key ]]; then
    # This is the important one. CREDENTIALS_KEY decrypts stored S3 secrets;
    # replacing it silently invalidates every access key that already exists.
    session_secret=$(read_env SESSION_SECRET || generate_secret)
    credentials_key=$existing_credentials_key
    postgres_password=$(read_env POSTGRES_PASSWORD || generate_url_safe_secret)
    good "Kept the existing secrets."
    muted "CREDENTIALS_KEY was preserved. Replacing it would make every existing"
    muted "S3 access key undecryptable, and they would all need reissuing."
  else
    session_secret=$(generate_secret)
    credentials_key=$(generate_secret)
    postgres_password=$(generate_url_safe_secret)
    good "Generated a session secret, a credentials key and a database password."
  fi

  # ── Review ─────────────────────────────────────────────────────────────────
  heading "Review"
  printf '  %-22s %s\n' "S3 API" "$public_s3"
  printf '  %-22s %s\n' "Console" "$public_console"
  printf '  %-22s %s\n' "Administrator" "$admin_email"
  printf '  %-22s %s\n' "Region" "$s3_region"
  printf '  %-22s %s\n' "Bucket subdomains" "${s3_domain:-off (path style only)}"
  printf '  %-22s %s\n' "Outbound email" "${resend_key:+configured}${resend_key:-not configured}"
  printf '  %-22s %s\n' "Ports bound to" "$bind_address"
  printf '\n'

  confirm "Write this to $ENV_FILE?" Y || fail "Cancelled. Nothing was written."

  # An existing file is moved aside rather than overwritten, because it holds
  # the only copy of the credentials key.
  if [[ -f $ENV_FILE ]]; then
    local backup="$ENV_FILE.backup.$(date +%Y%m%d-%H%M%S)"
    cp "$ENV_FILE" "$backup"
    chmod 600 "$backup"
    muted "Previous configuration saved as $backup"
  fi

  write_env_file
  # The file holds secrets in plaintext, so it is readable only by its owner.
  chmod 600 "$ENV_FILE"
  good "Wrote $ENV_FILE (readable only by you)"

  printf '\n'
  warn "Back up CREDENTIALS_KEY from $ENV_FILE."
  muted "It is not in a database dump. Without it, every stored S3 secret is"
  muted "undecryptable and all access keys have to be reissued."

  return 0
}

write_env_file() {
  cat > "$ENV_FILE" <<EOF
# Written by ./setup.sh on $(date '+%Y-%m-%d %H:%M:%S').
#
# This file contains secrets. It is chmod 600 and must not be committed.
# See .env.example for what every variable does.

ENV=production

# ─── Listeners ───────────────────────────────────────────────────────────────
# Ports inside the container. Both serve plain HTTP; TLS terminates at your
# reverse proxy.
S3_PORT=8443
CONSOLE_PORT=8444

# ─── Public identity ─────────────────────────────────────────────────────────
# The URLs clients actually reach you on. SigV4 signs the hostname, so these
# must match what clients use or every S3 request fails to verify.
PUBLIC_S3_URL=$public_s3
PUBLIC_CONSOLE_URL=$public_console
S3_DOMAIN=$s3_domain
S3_REGION=$s3_region

# ─── Database ────────────────────────────────────────────────────────────────
POSTGRES_PASSWORD=$postgres_password

# ─── Console ─────────────────────────────────────────────────────────────────
ADMIN_EMAIL=$admin_email

# Signs session cookies.
SESSION_SECRET=$session_secret

# Encrypts S3 secret keys at rest. BACK THIS UP — it is not in a database dump,
# and without it every access key must be reissued.
CREDENTIALS_KEY=$credentials_key

# ─── Email ───────────────────────────────────────────────────────────────────
RESEND_API_KEY=$resend_key
RESEND_FROM=$resend_from

# ─── Networking ──────────────────────────────────────────────────────────────
# Which addresses may set X-Forwarded-* headers. Narrow this to your proxy if
# you can: it is what stops an outside caller spoofing the public hostname.
TRUSTED_PROXIES=172.16.0.0/12,10.0.0.0/8,192.168.0.0/16

S3_BIND=$bind_address
CONSOLE_BIND=$bind_address

LOG_LEVEL=info
EOF
}

# ─── Starting ─────────────────────────────────────────────────────────────────

start_stack() {
  heading "Building and starting"
  muted "The first build compiles the console and the server, and takes a few minutes."
  printf '\n'

  # The version is stamped into the binary at build time from the git tag.
  # Compose cannot work it out for itself, so it is passed in here; without it
  # the build falls back to "dev" and the console shows that.
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)" \
    "${COMPOSE[@]}" up -d --build || fail "The stack failed to start. Check the output above."

  printf '\n'
  info "Waiting for the server to become healthy…"

  local container attempt=0
  container=$("${COMPOSE[@]}" ps -q s3d 2>/dev/null | head -1)
  [[ -n $container ]] || fail "The s3d container did not start. Try: ${COMPOSE[*]} logs s3d"

  # Health, not just "running": a container that is up but cannot reach its
  # database answers nothing useful, and saying "ready" then would be a lie.
  while (( attempt < 90 )); do
    local status
    status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container" 2>/dev/null || echo "")
    case $status in
      healthy|running)
        if [[ $status == healthy ]] || "${COMPOSE[@]}" exec -T s3d /usr/local/bin/s3d healthcheck >/dev/null 2>&1; then
          good "The server is up."
          return 0
        fi
        ;;
      exited|dead)
        printf '\n'
        "${COMPOSE[@]}" logs --tail 30 s3d
        fail "The server stopped. The log above should say why."
        ;;
    esac
    sleep 2
    ((attempt++))
  done

  printf '\n'
  "${COMPOSE[@]}" logs --tail 30 s3d
  fail "The server did not become healthy in three minutes. The log above should say why."
}

# ─── Afterwards ───────────────────────────────────────────────────────────────

offer_first_key() {
  heading "First access key"
  muted "The S3 API needs a key pair. One can be made here, or in the console later."
  printf '\n'

  confirm "Create one now?" Y || return 0

  printf '\n'
  # Printed straight through: the secret is shown once and is not recoverable,
  # so capturing and reformatting it risks losing it to a shell quoting bug.
  "${COMPOSE[@]}" exec -T s3d /usr/local/bin/s3d credential create "created by setup.sh" ||
    warn "Could not create a key. Make one in the console instead."
}

show_next_steps() {
  local console_url s3_url admin_email
  console_url=$(read_env PUBLIC_CONSOLE_URL || echo "http://localhost:8444")
  s3_url=$(read_env PUBLIC_S3_URL || echo "http://localhost:8443")
  admin_email=$(read_env ADMIN_EMAIL || echo "your-admin@example.com")

  heading "Ready"
  printf '  %-10s %s%s%s\n' "Console" "$CYAN" "$console_url" "$RESET"
  printf '  %-10s %s%s%s\n' "S3 API" "$CYAN" "$s3_url" "$RESET"

  heading "Signing in"
  # The admin account exists but has no password: nothing can invent one for
  # it, so the very first thing anyone must do is set it here.
  info "Set the administrator password, then sign in as $admin_email:"
  printf '\n    %s%s exec s3d s3d user set-password %s%s\n' \
    "$DIM" "${COMPOSE[*]}" "$admin_email" "$RESET"
  printf '\n'
  muted "It asks twice and does not echo, so it stays out of your shell history."

  heading "Everyday commands"
  # Width chosen to fit the longest command below; a narrower column would
  # leave the descriptions jagged.
  local width=46
  printf "    %-${width}s %s\n" "${COMPOSE[*]} logs -f s3d" "follow the log"
  printf "    %-${width}s %s\n" "${COMPOSE[*]} restart s3d" "restart"
  printf "    %-${width}s %s\n" "${COMPOSE[*]} down" "stop"
  printf "    %-${width}s %s\n" "${COMPOSE[*]} exec s3d s3d credential list" "list access keys"
  printf "    %-${width}s %s\n" "./setup.sh --configure" "change the configuration"

  if [[ $console_url != http://localhost* ]]; then
    heading "Reverse proxy"
    info "Point your proxy at the published ports, then read docs/reverse-proxy.md."
    muted "Four nginx defaults will otherwise leave a working console and a broken"
    muted "S3 API — the upload size cap is the first one you will hit."
  fi

  printf '\n'
  muted "Objects are stored as a single copy. There is no replication or repair."
  muted "Back up the data volume, a pg_dump, and CREDENTIALS_KEY together."
  printf '\n'
}

# ─── Entry point ──────────────────────────────────────────────────────────────

main() {
  local do_configure=true do_start=true

  case "${1:-}" in
    --configure) do_start=false ;;
    --start)     do_configure=false ;;
    -h|--help)
      cat <<'USAGE'
Pail setup

  ./setup.sh              ask the questions, write .env, build and start
  ./setup.sh --configure  write .env only
  ./setup.sh --start      start using the existing .env

Secrets are generated for you. Re-running preserves CREDENTIALS_KEY, because
replacing it would invalidate every S3 access key that already exists.
USAGE
      exit 0
      ;;
    "") ;;
    *) fail "Unknown option: $1 (try --help)" ;;
  esac

  check_prerequisites

  if $do_configure; then
    # A declined reconfigure returns non-zero, which is not an error — it just
    # means the existing file stands.
    configure || true
  fi

  [[ -f $ENV_FILE ]] || fail "No $ENV_FILE. Run ./setup.sh without --start first."

  if $do_start; then
    start_stack
    offer_first_key
    show_next_steps
  else
    heading "Configured"
    info "Start it with: ./setup.sh --start"
    printf '\n'
  fi
}

main "$@"
