#!/bin/sh
# Offline end-to-end demo. Only synthetic events and dry-run actions; no GitHub access.
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
DEMO=$(mktemp -d /tmp/github-botd-demo.XXXXXX)
PID=
cleanup() { if [ -n "$PID" ]; then kill "$PID" 2>/dev/null || true; wait "$PID" 2>/dev/null || true; fi; rm -rf "$DEMO"; }
trap cleanup EXIT INT TERM
export DEMO
python3 - <<'PY'
import json,os,secrets
from pathlib import Path
p=Path(os.environ['DEMO'])
for name in ('client','webhook'):
    f=p/name; f.write_text(secrets.token_hex(32)); f.chmod(0o600)
c={"version":1,"state_dir":"state","dry_run":True,"ipc":{"socket":"run/botd.sock"},"webhook":{"bind":"127.0.0.1:0","sources":{"demo":{"secret_file":"webhook"}}},"github":{},"repositories":{"demo":{"full_name":"example/project"}},"clients":{"demo":{"token_file":"client","repositories":["demo"],"capabilities":["events.read","events.replay","issue.comment.create"]}},"routes":[{"id":"mentions","repositories":["demo"],"events":["issue_comment"],"trigger":"mention","names":["botd"],"actors":["alice"],"clients":["demo"]}],"poll":{"enabled":False}}
(p/'config.json').write_text(json.dumps(c));(p/'config.json').chmod(0o600)
PY
"$ROOT/bin/github-botd" -config "$DEMO/config.json" >"$DEMO/daemon.log" 2>&1 &
PID=$!
export ROOT
python3 - <<'PY'
import hashlib,hmac,json,os,re,subprocess,time,urllib.request
from pathlib import Path
p=Path(os.environ['DEMO']); endpoint='unix://'+str(p/'run/botd.sock'); ctl=[os.environ['ROOT']+'/bin/github-botctl','-endpoint',endpoint,'-token-file',str(p/'client')]
for _ in range(100):
    logs=(p/'daemon.log').read_text()
    match=re.search(r'name=webhook address=(127\.0\.0\.1:\d+)',logs)
    if match and (p/'run/botd.sock').exists(): break
    time.sleep(.05)
else: raise RuntimeError('daemon did not start: '+logs)
body=json.dumps({'action':'created','repository':{'full_name':'example/project'},'sender':{'login':'alice','type':'User'},'issue':{'number':7},'comment':{'id':1,'body':'@botd inspect the local demo'}}).encode()
sig=hmac.new((p/'webhook').read_bytes(),body,hashlib.sha256).hexdigest()
req=urllib.request.Request('http://'+match[1]+'/hooks/demo',body,{'Content-Type':'application/json','X-GitHub-Event':'issue_comment','X-GitHub-Delivery':'offline-demo','X-Hub-Signature-256':'sha256='+sig})
with urllib.request.urlopen(req) as res: assert json.load(res)['new'] is True
with urllib.request.urlopen(req) as res: assert json.load(res)['new'] is False
reply=json.loads(subprocess.check_output(ctl+['-wait','0','events'])); d=reply['deliveries'][0]
a={'version':'github-botd/v1','idempotency_key':'demo-response','kind':'issue.comment.create','repository':'demo','event_id':d['event']['id'],'number':7,'body':'Local demo received. No real GitHub write.'}
r=json.loads(subprocess.check_output(ctl+['action'],input=json.dumps(a).encode()))
r2=json.loads(subprocess.check_output(ctl+['action'],input=json.dumps(a).encode()));assert r['id']==r2['id']
subprocess.check_output(ctl+['ack',d['id']],input=json.dumps({'lease_token':d['lease_token']}).encode())
for _ in range(100):
    receipt=json.loads(subprocess.check_output(ctl+['receipt',r['id']]))
    if receipt['state']=='succeeded': break
    time.sleep(.05)
assert receipt['state']=='succeeded' and receipt['result']['dry_run'] is True
print('PASS: signed webhook -> durable dedupe -> Unix client -> idempotent dry-run action -> ACK -> receipt')
print('No live GitHub access or writes performed.')
PY
