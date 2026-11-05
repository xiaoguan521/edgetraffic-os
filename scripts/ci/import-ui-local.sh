#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
: "${NODE_PATH:?Set NODE_PATH to a test-only installation containing playwright}"
node -e "require('playwright')"
pnpm --dir web install --frozen-lockfile
pnpm --dir web build
REPORT="${IMPORT_REPORT_DIR:-dist/test-reports/import-ui-local}"
mkdir -p "$REPORT"
LIVE="${IMPORT_E2E_LIVE:-0}"
CONSOLE_PORT="${CONSOLE_PORT:-28443}"
GRPC_PORT="${GRPC_PORT:-29443}"
NODE1_PORT="${NODE1_PORT:-28001}"
NODE2_PORT="${NODE2_PORT:-28002}"
NODE_IMAGE="${IMPORT_NODE_IMAGE:-ghcr.io/xiaoguan521/edgetraffic-os/node:latest}"
W=$(mktemp -d /tmp/etos-import-ui.XXXXXX)
PID=""
NET=""
cleanup() {
  if [[ -n "${NET:-}" ]]; then
    docker rm -f -v "$NET-node1" "$NET-node2" "$NET-backend" >/dev/null 2>&1 || true
    docker network rm "$NET" >/dev/null 2>&1 || true
  fi
  if [[ -n "${PID:-}" ]]; then
    kill "$PID" 2>/dev/null || true
    for _ in $(seq 1 30); do
      kill -0 "$PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -rf "$W"
}
trap cleanup EXIT
go build -mod=vendor -o "$W/lic" ./cmd/syapp-license-server
"$W/lic" keygen -kid etos-ci-ephemeral -out "$W/keys" > "$W/keygen.txt"
pub=$(python3 -c 'import re,sys;print(re.findall(r"\"([A-Za-z0-9+/=]{44})\"",open(sys.argv[1]).read())[-1])' "$W/keygen.txt")
go build -mod=vendor -ldflags "-X github.com/xiaoguan521/edgetraffic-os/internal/license.ciKey=$pub" -o "$W/console" ./cmd/syapp-console
console_args=(-listen "127.0.0.1:$CONSOLE_PORT" -grpc-listen "0.0.0.0:$GRPC_PORT" -grpc-hosts host.docker.internal,localhost,127.0.0.1 -grpc-advertise "host.docker.internal:$GRPC_PORT" -data "$W/data")
if [[ -n "${IMPORT_TEST_DB:-}" ]]; then console_args+=(-db "$IMPORT_TEST_DB"); fi
"$W/console" "${console_args[@]}" > "$W/console.log" 2>&1 &
PID=$!

for _ in $(seq 1 40); do if curl --noproxy '*' -sf http://127.0.0.1:$CONSOLE_PORT/healthz > /dev/null; then break; fi; sleep 0.25; done
curl --noproxy '*' -sf -H 'X-Requested-With: etos' -H 'Content-Type: application/json' -d '{"username":"admin","password":"Demo@2026!"}' http://127.0.0.1:$CONSOLE_PORT/api/v1/setup > /dev/null
inst=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["installation_id"])' "$W/data/installation.json")
"$W/lic" issue -key "$W/keys/etos-ci-ephemeral.key" -kid etos-ci-ephemeral -installation-id "$inst" -binding installation_only -edition professional -customer import-ui -days 365 > "$W/license.lic"
BASE="http://127.0.0.1:$CONSOLE_PORT" python3 - "$W/license.lic" <<'PY'
import http.cookiejar,json,sys,urllib.request,os
op=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
def post(path,data):
 r=urllib.request.Request(os.environ['BASE']+'/api/v1'+path,data=json.dumps(data).encode(),headers={'Content-Type':'application/json','X-Requested-With':'etos'})
 return json.load(op.open(r))
post('/auth/login',{'username':'admin','password':'Demo@2026!'})
assert post('/license/import',{'raw':open(sys.argv[1]).read()})['state']=='VALID'
PY

if [[ "$LIVE" == 1 ]]; then
  NET="etos-import-$(basename "$W" | tr '[:upper:].' '[:lower:]-')"
  docker image inspect "$NODE_IMAGE" >/dev/null 2>&1 || docker pull "$NODE_IMAGE"
  arch=$(docker image inspect "$NODE_IMAGE" --format '{{.Architecture}}')
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=vendor -ldflags "-X github.com/xiaoguan521/edgetraffic-os/internal/license.ciKey=$pub" -o "$W/agent" ./cmd/syapp-agent
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=vendor -o "$W/backend" ./demo/backend
  docker network create --subnet "${IMPORT_TEST_SUBNET:-172.29.231.0/24}" "$NET" >/dev/null
  docker run -d --name "$NET-backend" --network "$NET" -v "$W/backend:/usr/bin/demo-backend:ro" --entrypoint /usr/bin/demo-backend "$NODE_IMAGE" >/dev/null
  BACKEND_IP=$(docker inspect "$NET-backend" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
  host_args=(--label edgetraffic.test=import-acceptance)
  if [[ "$(uname -s)" == Linux ]]; then host_args+=(--add-host host.docker.internal:host-gateway); fi
  for n in 1 2; do
    if [[ "$n" == 1 ]]; then port=$NODE1_PORT; else port=$NODE2_PORT; fi
    docker run -d --name "$NET-node$n" --hostname "import-node$n" --network "$NET" "${host_args[@]}" -p "127.0.0.1:$port:80" -v "$W/agent:/usr/bin/syapp-agent:ro" "$NODE_IMAGE" >/dev/null
  done
  BASE="http://127.0.0.1:$CONSOLE_PORT" TEST_NET="$NET" python3 - <<'PYLIVE'
import os,json,http.cookiejar,urllib.request,subprocess,time
op=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
def call(path,data=None):
 r=urllib.request.Request(os.environ['BASE']+'/api/v1'+path,data=json.dumps(data).encode() if data is not None else None,headers={'Content-Type':'application/json','X-Requested-With':'etos'})
 return json.load(op.open(r))
call('/auth/login',{'username':'admin','password':'Demo@2026!'})
for n in [1,2]:
 name=os.environ['TEST_NET']+'-node'+str(n)
 for attempt in range(60):
  if subprocess.run(['docker','exec',name,'test','-e','/opt/syapp/conf/current'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL).returncode==0:break
  time.sleep(.5)
 else:raise RuntimeError('node bootstrap timed out')
 e=call('/node-groups/1/enroll',{})
 result=subprocess.run(['docker','exec',name,'syapp-agent','enroll','-server',e['server'],'-token',e['token'],'-ca-fingerprint',e['ca_fingerprint']],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 if result.returncode:raise RuntimeError('node enrollment failed; enrollment token suppressed')
for attempt in range(120):
 if sum(n['status']=='online' for n in call('/nodes'))==2:break
 time.sleep(.5)
else:raise RuntimeError('two nodes did not connect')
PYLIVE
  export IMPORT_UPSTREAM="$BACKEND_IP:9001" IMPORT_NODE_URLS="http://127.0.0.1:$NODE1_PORT,http://127.0.0.1:$NODE2_PORT"
  docker image inspect "$NODE_IMAGE" --format '{{json .RepoDigests}}' > "$REPORT/node-image-digests.json"
fi
BASE="http://127.0.0.1:$CONSOLE_PORT" IMPORT_E2E_LIVE="$LIVE" LIC_TOOL="$W/lic" LIC_KEY="$W/keys/etos-ci-ephemeral.key" LIC_KID=etos-ci-ephemeral NO_PROXY='*' no_proxy='*' IMPORT_REPORT_DIR="$REPORT" make test-import-ui
