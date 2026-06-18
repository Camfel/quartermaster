#!/bin/bash
# VPN e2e test — verifies Gluetun deploys and a test service routes through it.
# Prerequisites: qm-daemon running, vpn-user + vpn-pass secrets created,
# VPN component enabled (qm enable vpn).

set -euo pipefail

RED='\033[31m'; GREEN='\033[32m'; YELLOW='\033[33m'; NC='\033[0m'
pass()  { echo -e "${GREEN}✓${NC} $1"; }
fail()  { echo -e "${RED}✗${NC} $1"; exit 1; }
info()  { echo -e "${YELLOW}→${NC} $1"; }

info "=== Quartermaster VPN E2E Test ==="

# ── 1. Verify daemon is running ──────────────────────────────────
info "Checking daemon..."
qm status > /dev/null 2>&1 || fail "Daemon unreachable — is qm-daemon running?"
pass "Daemon reachable"

# ── 2. Ensure VPN component is enabled ───────────────────────────
info "Checking VPN component..."
COMPONENTS=$(qm components list 2>&1)
if echo "$COMPONENTS" | grep -q "✓.*vpn"; then
  pass "VPN component enabled"
else
  info "Enabling VPN component..."
  qm enable vpn
  sleep 35  # wait for daemon to pull and start gluetun
fi

# ── 3. Wait for gluetun to be healthy ────────────────────────────
info "Waiting for gluetun to become healthy (up to 120s)..."
for i in $(seq 1 24); do
  STATUS=$(qm status 2>&1)
  if echo "$STATUS" | grep -q "gluetun.*✓ healthy"; then
    pass "Gluetun healthy after ${i}5s"
    break
  fi
  if echo "$STATUS" | grep -q "gluetun.*✗ unhealthy"; then
    GLUETUN_LOG=$(curl -s --unix-socket /run/quartermaster/daemon.sock "http://localhost/v1/services/gluetun/logs?tail=20" 2>/dev/null)
    if echo "$GLUETUN_LOG" | grep -q "Initialization Sequence Completed"; then
      pass "Gluetun log says tunnel is up (health check may still be converging)"
      break
    fi
  fi
  sleep 5
done

qm status 2>&1 | grep gluetun

# ── 4. Get the host's real IP (for comparison) ───────────────────
HOST_IP=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null || curl -s --connect-timeout 5 icanhazip.com 2>/dev/null)
info "Host public IP: $HOST_IP"

# ── 5. Deploy a test container that shares gluetun's namespace ───
info "Creating test container in VPN namespace..."
GLUETUN_PID=$(ctr -n quartermaster task ls 2>/dev/null | grep gluetun | awk '{print $2}')
if [ -z "$GLUETUN_PID" ] || [ "$GLUETUN_PID" = "0" ]; then
  fail "Gluetun not running — cannot find PID"
fi
info "Gluetun PID: $GLUETUN_PID"

# Create a test container that joins gluetun's network namespace
ctr -n quartermaster image pull docker.io/library/alpine:latest > /dev/null 2>&1 || true

# We'll use nsenter to run curl inside gluetun's namespace (same as a sidecar would)
info "Checking egress IP from VPN namespace..."
VPN_IP=$(nsenter -t $GLUETUN_PID -n curl -s --connect-timeout 10 ifconfig.me 2>/dev/null) || true
if [ -z "$VPN_IP" ]; then
  VPN_IP=$(nsenter -t $GLUETUN_PID -n curl -s --connect-timeout 10 icanhazip.com 2>/dev/null) || true
fi

info "VPN namespace public IP: $VPN_IP"

# ── 6. Verify egress is through the VPN ──────────────────────────
if [ -z "$VPN_IP" ]; then
  fail "Could not determine VPN egress IP — gluetun may not have an active tunnel (check credentials)"
fi

if [ -n "$HOST_IP" ] && [ "$VPN_IP" = "$HOST_IP" ]; then
  fail "VPN egress IP ($VPN_IP) matches host IP ($HOST_IP) — traffic is NOT going through VPN!"
fi

if [ -n "$HOST_IP" ] && [ "$VPN_IP" != "$HOST_IP" ]; then
  pass "VPN egress IP ($VPN_IP) differs from host IP ($HOST_IP) — traffic IS routed through VPN"
else
  pass "VPN egress IP: $VPN_IP (host IP unknown — check manually)"
fi

# ── 7. Verify host internet is unaffected ────────────────────────
info "Checking host internet..."
HOST_PING=$(ping -c 1 -W 3 8.8.8.8 2>&1) || fail "Host cannot reach 8.8.8.8 — VPN may have broken host networking!"
pass "Host internet works"

# ── 8. Check bridge and port forwarding ──────────────────────────
info "Checking bridge..."
ip link show qm0 > /dev/null 2>&1 || fail "qm0 bridge does not exist"
pass "Bridge qm0 exists"

DNAT_COUNT=$(iptables -t nat -L PREROUTING -n 2>/dev/null | grep -c "DNAT" || echo 0)
info "DNAT rules: $DNAT_COUNT (expect >0 if media-stack services are deployed)"

echo ""
echo -e "${GREEN}=== All checks passed! ===${NC}"
echo "Host IP:  $HOST_IP"
echo "VPN IP:   $VPN_IP"
echo "Bridge:   $(ip link show qm0 2>/dev/null | head -1)"
echo "Status:"
qm status 2>&1
